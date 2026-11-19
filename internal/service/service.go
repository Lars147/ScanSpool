// Package service owns scanner transactions, configuration and completed uploads.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"scanspool/internal/config"
	"scanspool/internal/output"
	"scanspool/internal/scanner"
	"scanspool/internal/setup"
	"scanspool/internal/vens"
	"scanspool/profiles"
)

var ErrBusy = errors.New("scanner or configuration operation is busy")

type Status struct {
	Version            string `json:"version"`
	Running            bool   `json:"running"`
	Mode               string `json:"mode"`
	Configured         bool   `json:"configured"`
	Online             bool   `json:"online"`
	Host               string `json:"host,omitempty"`
	Model              string `json:"model,omitempty"`
	Serial             string `json:"serial,omitempty"`
	Firmware           string `json:"firmware,omitempty"`
	WiFi               uint32 `json:"wifi"`
	PaperLoaded        *bool  `json:"paperLoaded,omitempty"`
	Busy               bool   `json:"busy"`
	RecoveryRequired   bool   `json:"recoveryRequired"`
	FTPReady           bool   `json:"ftpReady"`
	OutputReady        bool   `json:"outputReady"`
	OutputKind         string `json:"outputKind"`
	OutputDirectory    string `json:"outputDirectory,omitempty"`
	OtherTargetPending int    `json:"otherTargetPending"`
	Pending            int    `json:"pending"`
	LastPages          int    `json:"lastPages"`
	LastFile           string `json:"lastFile,omitempty"`
	LastScan           string `json:"lastScan,omitempty"`
	Error              string `json:"error,omitempty"`
	ErrorHint          string `json:"errorHint,omitempty"`
	ScannerError       string `json:"scannerError,omitempty"`
}
type API interface {
	Status(context.Context) (Status, error)
	Settings(context.Context) (config.View, error)
	Save(context.Context, config.Update) (config.View, error)
	Profile(context.Context) (config.ProfileView, error)
	SaveProfile(context.Context, config.ProfileView) (config.ProfileView, error)
	Discover(context.Context, string) ([]setup.Candidate, error)
	Connect(context.Context, setup.Request) error
	Recover(context.Context) error
	Retry(context.Context) error
	StartScan(context.Context) error
}

type scanSource interface {
	Scan(vens.ScanConfig, func(vens.Page)) ([]vens.Page, error)
}

type connectedScanner interface {
	scanSource
	Online() bool
	Serial() string
	FirmwareRevision() string
	WifiState() uint32
	ConnectionError() string
	CheckADFStatus() (*vens.ADFStatus, error)
	StopReconnectLoop()
	Disconnect()
}

type runtime struct {
	sc     connectedScanner
	button *scanner.ButtonListener
	cancel context.CancelFunc
}

func (r *runtime) Close() { r.cancel(); r.sc.StopReconnectLoop(); r.button.Stop(); r.sc.Disconnect() }

type App struct {
	dir, version, mode            string
	ctx                           context.Context
	cancel                        context.CancelFunc
	store                         *setup.Store
	settings                      *config.Store
	manager                       *setup.Manager
	guard                         *scanner.ScanGuard
	mu                            sync.RWMutex
	current                       *runtime
	running                       bool
	paper                         *bool
	scannerError                  string
	lastError, lastFile, lastScan string
	lastErrorHint                 string
	lastPages                     int
	workers                       sync.WaitGroup
	closeOnce                     sync.Once
}

func New(parent context.Context, dir, version, mode string) (*App, error) {
	store, err := setup.NewStore(dir)
	if err != nil {
		return nil, err
	}
	settings, err := config.Open(dir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	app := &App{dir: dir, version: version, mode: mode, ctx: ctx, cancel: cancel, store: store, settings: settings, manager: setup.NewManager(store), guard: scanner.NewScanGuard()}
	app.manager.Activate = app.change
	return app, nil
}
func (a *App) Start() error {
	a.mu.Lock()
	a.running = true
	a.mu.Unlock()
	release, ok := a.guard.TryAcquire()
	if !ok {
		return ErrBusy
	}
	err := a.startSaved()
	release()
	if err != nil {
		a.setError(err)
	}
	a.workers.Add(1)
	go a.poll()
	return err
}
func (a *App) startSaved() error {
	connection := a.store.Get()
	if connection == nil || a.manager.Status().RecoveryRequired || a.ctx.Err() != nil {
		return nil
	}
	if connection.Mode != "pc" {
		return fmt.Errorf("only iX1300 PC connections are supported")
	}
	ctx, cancel := context.WithCancel(a.ctx)
	sc := scanner.New(connection.Host, vens.DefaultDataPort, vens.DefaultControlPort, "", scanner.WithSerial(connection.Serial), scanner.WithPCConfig(connection.Template, profiles.IX1300()), scanner.WithScanGuard(a.guard))
	r := &runtime{sc: sc, cancel: cancel}
	r.button = scanner.NewButtonListener(connection.Host, func() { a.startScan(sc) })
	if err := r.button.Start(ctx); err != nil {
		cancel()
		return err
	}
	sc.StartReconnectLoop(ctx)
	a.mu.Lock()
	a.current = r
	a.paper = nil
	a.scannerError = ""
	a.mu.Unlock()
	return nil
}
func (a *App) stopCurrent() {
	a.mu.Lock()
	current := a.current
	a.current = nil
	a.paper = nil
	a.mu.Unlock()
	if current != nil {
		current.Close()
	}
}
func (a *App) change(ctx context.Context, fn func() error) error {
	release, ok := a.guard.TryAcquire()
	if !ok {
		return ErrBusy
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	a.stopCurrent()
	operationErr := fn()
	activationErr := a.startSaved()
	if activationErr != nil {
		a.setError(activationErr)
	}
	if operationErr == nil && activationErr != nil {
		return fmt.Errorf("scanner registration saved, activation failed: %w", activationErr)
	}
	return errors.Join(operationErr, activationErr)
}
func (a *App) setError(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.lastError = err.Error()
		a.lastErrorHint = "Scan or delivery failed. Open Scanner status for details."
		if errors.Is(err, os.ErrPermission) {
			a.lastErrorHint = "Permission denied. Check file and folder access rights."
			if hint := config.LocalPermissionHint(err); hint != "" {
				a.lastErrorHint = hint
			}
		}
	} else {
		a.lastError = ""
		a.lastErrorHint = ""
	}
}
func (a *App) Close() {
	a.closeOnce.Do(func() {
		a.cancel()
		a.guard.StopAndWait()
		a.stopCurrent()
		a.workers.Wait()
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()
	})
}

func (a *App) Status(context.Context) (Status, error) {
	state := a.manager.Status()
	target := a.settings.Get()
	status := Status{Version: a.version, Mode: a.mode, Configured: state.Configured, Host: state.Host, Model: state.Model, RecoveryRequired: state.RecoveryRequired, Busy: a.guard.Busy(), OutputKind: target.OutputKind(), OutputReady: target.Validate() == nil}
	status.FTPReady = status.OutputKind == config.KindFTP && status.OutputReady
	if status.OutputKind == config.KindLocal {
		status.OutputDirectory = target.LocalDirectory
	} else {
		status.OutputDirectory = target.Directory
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	status.Running = a.running
	status.PaperLoaded = a.paper
	status.ScannerError = a.scannerError
	status.LastPages, status.LastFile, status.LastScan, status.Error = a.lastPages, a.lastFile, a.lastScan, a.lastError
	status.ErrorHint = a.lastErrorHint
	if current := a.current; current != nil {
		status.Online = current.sc.Online()
		if !status.Online {
			status.PaperLoaded = nil
		}
		status.Serial = current.sc.Serial()
		status.Firmware = current.sc.FirmwareRevision()
		status.WiFi = current.sc.WifiState()
		if status.ScannerError == "" {
			status.ScannerError = current.sc.ConnectionError()
		}
	}
	if status.Serial == "" {
		if connection := a.store.Get(); connection != nil {
			status.Serial = connection.Serial
		}
	}
	jobs, err := output.Pending(a.dir)
	if err != nil {
		return status, err
	}
	status.Pending = len(jobs)
	for _, job := range jobs {
		if !target.SameTarget(job.Target) {
			status.OtherTargetPending++
		}
	}
	return status, nil
}
func (a *App) Settings(context.Context) (config.View, error) { return a.settings.View(), nil }
func (a *App) Save(ctx context.Context, update config.Update) (config.View, error) {
	release, ok := a.guard.TryAcquire()
	if !ok {
		return config.View{}, ErrBusy
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return config.View{}, err
	}
	view, err := a.settings.Save(update)
	if hint := config.LocalPermissionHint(err); hint != "" {
		err = fmt.Errorf("%s\nDetails: %w", hint, err)
	}
	return view, err
}
func (a *App) Profile(context.Context) (config.ProfileView, error) {
	return a.settings.Profile(), nil
}

func (a *App) SaveProfile(ctx context.Context, update config.ProfileView) (config.ProfileView, error) {
	release, ok := a.guard.TryAcquire()
	if !ok {
		return config.ProfileView{}, ErrBusy
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return config.ProfileView{}, err
	}
	return a.settings.SaveProfile(update)
}

func (a *App) Discover(ctx context.Context, ip string) ([]setup.Candidate, error) {
	release, ok := a.guard.TryAcquire()
	if !ok {
		return nil, ErrBusy
	}
	defer release()
	return a.manager.Discover(ctx, ip)
}
func (a *App) Connect(ctx context.Context, request setup.Request) error {
	return a.manager.Connect(ctx, request)
}
func (a *App) Recover(ctx context.Context) error {
	if !a.manager.Status().RecoveryRequired {
		return fmt.Errorf("no registration recovery pending")
	}
	return a.manager.Recover(ctx)
}
func (a *App) Retry(ctx context.Context) error {
	release, ok := a.guard.TryAcquire()
	if !ok {
		return ErrBusy
	}
	defer release()
	err := a.sendPending(ctx)
	a.setError(err)
	return err
}
func (a *App) sendPending(ctx context.Context) error {
	jobs, err := output.Pending(a.dir)
	if err != nil {
		return err
	}
	target := a.settings.Get()
	var failures []error
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if !target.SameTarget(job.Target) {
			continue
		}
		if err := output.Send(ctx, a.dir, target, job); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", job.Name, err))
			continue
		}
		a.mu.Lock()
		a.lastFile = job.Name
		a.mu.Unlock()
	}
	return errors.Join(failures...)
}

// StartScan accepts one scan on the receiver's current connection. Once accepted,
// the receiver owns its complete lifetime, independent of the requesting client.
func (a *App) StartScan(ctx context.Context) error {
	release, ok := a.guard.TryAcquire()
	if !ok {
		return ErrBusy
	}
	// launchScan takes over the lease only after all preconditions pass.
	sc, err := a.scannerForScan(ctx)
	if err != nil {
		release()
		return err
	}
	return a.launchScan(sc, release)
}

func (a *App) scannerForScan(ctx context.Context) (connectedScanner, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state := a.manager.Status()
	if state.RecoveryRequired {
		return nil, fmt.Errorf("recover the previous scanner registration first")
	}
	if !state.Configured {
		return nil, fmt.Errorf("configure a scanner with setup first")
	}
	a.mu.RLock()
	current, running := a.current, a.running
	a.mu.RUnlock()
	if !running || current == nil {
		return nil, fmt.Errorf("start the receiver with run or setup first")
	}
	if !current.sc.Online() {
		return nil, fmt.Errorf("scanner is offline or connecting; switch it on and retry when online")
	}
	return current.sc, nil
}

func (a *App) startScan(sc scanSource) {
	release, ok := a.guard.TryAcquire()
	if !ok {
		return
	}
	// Errors are recorded by launchScan before it releases the operation lease.
	_ = a.launchScan(sc, release)
}

// launchScan consumes release on both failure and success; a successful worker
// keeps it until PDF publication and delivery have completed.
func (a *App) launchScan(sc scanSource, release func()) error {
	if err := a.ctx.Err(); err != nil {
		release()
		return err
	}
	target := a.settings.Get()
	if err := target.Validate(); err != nil {
		a.setError(err)
		release()
		return err
	}
	if target.OutputKind() == config.KindLocal {
		if err := config.CheckLocalDirectory(target.LocalDirectory, filepath.Join(a.dir, "outbox")); err != nil {
			err = fmt.Errorf("cannot write to local output folder %q: %w", target.LocalDirectory, err)
			a.setError(err)
			release()
			return err
		}
	}
	profile := a.settings.Profile().Profile
	wire := profile
	// Keep the scanner's page sequence intact. Filter blanks only after
	// receiving the complete batch.
	wire.BlankPageRemoval = false
	a.mu.Lock()
	a.lastError, a.lastFile = "", ""
	a.lastErrorHint = ""
	a.lastPages = 0
	a.lastScan = time.Now().UTC().Format(time.RFC3339)
	a.mu.Unlock()
	a.workers.Add(1)
	go func() {
		defer a.workers.Done()
		defer release()
		pages, err := sc.Scan(wire, func(page vens.Page) {
			if len(page.JPEG) == 0 {
				return
			}
			a.mu.Lock()
			a.lastPages++
			a.mu.Unlock()
		})
		if err == nil && profile.BlankPageRemoval {
			pages, err = output.RemoveBlankPages(a.ctx, pages)
		}
		a.mu.Lock()
		a.lastPages = len(pages)
		a.mu.Unlock()
		if err != nil {
			a.setError(err)
			return
		} // Never generate or queue a partial scan.
		if len(pages) == 0 {
			return
		}
		data, err := output.PDF(pages)
		if err != nil {
			a.setError(err)
			return
		}
		name, err := output.Enqueue(a.dir, target, data)
		if err != nil {
			a.setError(err)
			return
		}
		a.mu.Lock()
		a.lastFile = name
		a.mu.Unlock()
		err = a.sendPending(a.ctx)
		a.setError(err)
	}()
	return nil
}
func (a *App) poll() {
	defer a.workers.Done()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	uploadTicks := 0
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
		}
		release, ok := a.guard.TryAcquire()
		if !ok {
			continue
		}
		if a.ctx.Err() != nil {
			release()
			return
		}
		a.mu.RLock()
		current := a.current
		a.mu.RUnlock()
		// A competing application may have held the button port at startup.
		// Retry activation after it releases the port without restarting the service.
		if current == nil {
			if err := a.startSaved(); err != nil {
				a.setError(err)
			} else {
				a.mu.RLock()
				current = a.current
				a.mu.RUnlock()
				if current != nil {
					a.setError(nil)
				}
			}
		}
		if current != nil && current.sc.Online() {
			state, err := current.sc.CheckADFStatus()
			a.mu.Lock()
			if err != nil {
				a.scannerError = err.Error()
				a.paper = nil
			} else {
				paper := state.HasPaper
				a.paper = &paper
				a.scannerError = ""
			}
			a.mu.Unlock()
		}
		uploadTicks++
		if uploadTicks >= 5 {
			uploadTicks = 0
			jobs, err := output.Pending(a.dir)
			if err != nil {
				a.setError(err)
			} else if len(jobs) > 0 {
				a.setError(a.sendPending(a.ctx))
			}
		}
		release()
	}
}
