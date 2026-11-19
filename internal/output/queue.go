package output

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"scanspool/internal/config"
	"scanspool/internal/persist"
)

type Job struct {
	Name   string             `json:"name"`
	Target config.Destination `json:"target"`
}

func queueDir(dir string) string { return filepath.Join(dir, "outbox") }
func Enqueue(dir string, target config.Destination, pdf []byte) (string, error) {
	if err := target.Validate(); err != nil {
		return "", err
	}
	root := queueDir(dir)
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	name := "scan_" + time.Now().UTC().Format("20060102_150405") + "_" + hex.EncodeToString(random[:]) + ".pdf"
	if err := persist.WriteFile(filepath.Join(root, name), pdf); err != nil {
		return "", err
	}
	// Credentials remain in settings.json, not in queued job metadata.
	target.Password = ""
	target.Kind = target.OutputKind()
	if target.Kind == config.KindLocal {
		target.FTP = config.FTP{}
	} else {
		target.LocalDirectory = ""
	}
	data, err := json.Marshal(Job{Name: name, Target: target})
	if err != nil {
		return name, err
	}
	if err := persist.WriteFile(filepath.Join(root, name+".json"), data); err != nil {
		return name, err
	}
	return name, nil
}
func Pending(dir string) ([]Job, error) {
	paths, err := filepath.Glob(filepath.Join(queueDir(dir), "*.pdf.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	jobs := make([]Job, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var job Job
		if err := json.Unmarshal(data, &job); err != nil {
			return nil, fmt.Errorf("invalid queued job %s: %w", filepath.Base(path), err)
		}
		if !validJobName(job.Name) || job.Name+".json" != filepath.Base(path) {
			return nil, fmt.Errorf("invalid queued filename")
		}
		if err := job.Target.Validate(); err != nil {
			return nil, fmt.Errorf("invalid queued target %s: %w", job.Name, err)
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}
func validJobName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\") && filepath.Base(name) == name
}

func Send(ctx context.Context, dir string, target config.Destination, job Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validJobName(job.Name) {
		return fmt.Errorf("invalid queued filename")
	}
	if !target.SameTarget(job.Target) {
		return fmt.Errorf("queued PDF belongs to a different output target; restore that target to retry (file retained in outbox)")
	}
	file, err := os.Open(filepath.Join(queueDir(dir), job.Name))
	if err != nil {
		return err
	}
	defer file.Close()
	var deliveryErr error
	switch target.OutputKind() {
	case config.KindFTP:
		deliveryErr = Upload(ctx, target.FTP, job.Name, file)
	case config.KindLocal:
		deliveryErr = Local(ctx, dir, target.LocalDirectory, job.Name, file)
	default:
		deliveryErr = fmt.Errorf("unknown output kind %q", target.Kind)
	}
	if deliveryErr != nil {
		return deliveryErr
	}
	// Close the PDF before removal, including on Windows.
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(queueDir(dir), job.Name+".json")); err != nil {
		return err
	}
	return os.Remove(filepath.Join(queueDir(dir), job.Name))
}
