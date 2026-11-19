package output

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"scanspool/internal/config"
)

type ftpFixture struct {
	mu         sync.Mutex
	files      map[string][]byte
	commands   []string
	failRename bool
}

func ftpServer(t *testing.T, failRename bool) (config.FTP, *ftpFixture) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &ftpFixture{files: map[string][]byte{}, failRename: failRename}
	var workers sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				writer := func(code int, text string) { fmt.Fprintf(conn, "%d %s\r\n", code, text) }
				writer(220, "test FTP")
				reader := bufio.NewReader(conn)
				var passive net.Listener
				var rename string
				defer func() {
					if passive != nil {
						passive.Close()
					}
				}()
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimRight(line, "\r\n")
					command, arg, _ := strings.Cut(line, " ")
					fixture.mu.Lock()
					fixture.commands = append(fixture.commands, line)
					fixture.mu.Unlock()
					switch command {
					case "USER":
						writer(331, "password")
					case "PASS":
						writer(230, "logged in")
					case "FEAT":
						io.WriteString(conn, "211-Features\r\n EPSV\r\n UTF8\r\n211 End\r\n")
					case "OPTS", "TYPE", "NOOP":
						writer(200, "OK")
					case "CWD":
						writer(250, "changed")
					case "PWD":
						writer(257, "\"/consume\"")
					case "EPSV":
						passive, err = net.Listen("tcp4", "127.0.0.1:0")
						if err != nil {
							return
						}
						writer(229, fmt.Sprintf("Entering Extended Passive Mode (|||%d|)", passive.Addr().(*net.TCPAddr).Port))
					case "STOR":
						writer(150, "send file")
						dataConn, err := passive.Accept()
						if err != nil {
							return
						}
						dataConn.SetDeadline(time.Now().Add(2 * time.Second))
						data, err := io.ReadAll(dataConn)
						dataConn.Close()
						passive.Close()
						passive = nil
						if err != nil {
							return
						}
						fixture.mu.Lock()
						fixture.files[arg] = data
						fixture.mu.Unlock()
						writer(226, "complete")
					case "RNFR":
						rename = arg
						writer(350, "rename pending")
					case "RNTO":
						if fixture.failRename {
							writer(550, "synthetic rename failure")
						} else {
							fixture.mu.Lock()
							fixture.files[arg] = fixture.files[rename]
							delete(fixture.files, rename)
							fixture.mu.Unlock()
							writer(250, "renamed")
						}
					case "QUIT":
						writer(221, "bye")
						return
					default:
						writer(502, "unsupported")
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done; workers.Wait() })
	return config.FTP{Host: listener.Addr().String(), User: "scan", Password: "test", Directory: "/consume"}, fixture
}
func TestFTPPublishesByRenameAfterCompleteUpload(t *testing.T) {
	target, server := ftpServer(t, false)
	data := []byte("%PDF-1.7\ncomplete document")
	if err := Upload(context.Background(), target, "scan.pdf", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if !bytes.Equal(server.files["scan.pdf"], data) || server.files[".scan.pdf.partial"] != nil {
		t.Fatal("final document incorrect")
	}
	commands := strings.Join(server.commands, "\n")
	for _, expected := range []string{"CWD /consume", "STOR .scan.pdf.partial", "RNFR .scan.pdf.partial", "RNTO scan.pdf"} {
		if !strings.Contains(commands, expected) {
			t.Fatal("missing", expected, commands)
		}
	}
}
func TestQueueRetainsDocumentOnFTPFailureAndBindsDestination(t *testing.T) {
	target, server := ftpServer(t, true)
	dir := t.TempDir()
	name, err := Enqueue(dir, config.Destination{FTP: target}, []byte("%PDF complete"))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := Pending(dir)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	if jobs[0].Target.Password != "" {
		t.Fatal("queued metadata contains password")
	}
	if err := Send(context.Background(), dir, config.Destination{FTP: target}, jobs[0]); err == nil {
		t.Fatal("failed publish became success")
	}
	if _, err := os.Stat(filepath.Join(dir, "outbox", name)); err != nil {
		t.Fatal("lost recoverable PDF", err)
	}
	server.mu.Lock()
	if server.files[name] != nil {
		t.Error("published failed upload")
	}
	server.failRename = false
	server.mu.Unlock()
	wrong := target
	wrong.Directory = "/other"
	if err := Send(context.Background(), dir, config.Destination{FTP: wrong}, jobs[0]); err == nil {
		t.Fatal("silently rerouted queued document")
	}
	if err := Send(context.Background(), dir, config.Destination{FTP: target}, jobs[0]); err != nil {
		t.Fatal(err)
	}
	jobs, err = Pending(dir)
	if err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
}
func TestFTPCancellationInterruptsWelcome(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	target := config.FTP{Host: listener.Addr().String(), User: "scan", Directory: "/"}
	start := time.Now()
	err = Upload(ctx, target, "scan.pdf", strings.NewReader("PDF"))
	if err == nil || time.Since(start) > time.Second {
		t.Fatal("FTP cancellation did not interrupt read", err)
	}
	select {
	case conn := <-accepted:
		conn.Close()
	case <-time.After(time.Second):
		t.Fatal("no FTP connection")
	}
}
