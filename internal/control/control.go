// Package control provides authenticated local-only administration for the TUI.
// It is not a public HTTP/eSCL or browser interface.
package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"scanspool/internal/config"
	"scanspool/internal/persist"
	"scanspool/internal/service"
	"scanspool/internal/setup"
)

var ErrUnavailable = errors.New("no running ScanSpool service")

type endpoint struct {
	Address  string `json:"address"`
	Token    string `json:"token"`
	Protocol int    `json:"protocol"`
}
type failure struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}
type Server struct {
	server   *http.Server
	listener net.Listener
	dir      string
	info     endpoint
}

func endpointPath(dir string) string { return filepath.Join(dir, "control.json") }
func reply(w http.ResponseWriter, value any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		code := "operation_failed"
		if errors.Is(err, service.ErrBusy) {
			code = "busy"
		}
		if errors.Is(err, config.ErrConflict) {
			code = "conflict"
		}
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(failure{err.Error(), code})
		return
	}
	json.NewEncoder(w).Encode(value)
}
func decode(r *http.Request, value any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}
func handler(api service.API, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) { v, e := api.Status(r.Context()); reply(w, v, e) })
	mux.HandleFunc("GET /v1/settings", func(w http.ResponseWriter, r *http.Request) { v, e := api.Settings(r.Context()); reply(w, v, e) })
	mux.HandleFunc("PUT /v1/settings", func(w http.ResponseWriter, r *http.Request) {
		var v config.Update
		if err := decode(r, &v); err != nil {
			reply(w, nil, err)
			return
		}
		result, err := api.Save(r.Context(), v)
		reply(w, result, err)
	})
	mux.HandleFunc("GET /v1/profile", func(w http.ResponseWriter, r *http.Request) {
		view, err := api.Profile(r.Context())
		reply(w, view, err)
	})
	mux.HandleFunc("PUT /v1/profile", func(w http.ResponseWriter, r *http.Request) {
		var update config.ProfileView
		if err := decode(r, &update); err != nil {
			reply(w, nil, err)
			return
		}
		view, err := api.SaveProfile(r.Context(), update)
		reply(w, view, err)
	})
	mux.HandleFunc("GET /v1/discover", func(w http.ResponseWriter, r *http.Request) {
		v, e := api.Discover(r.Context(), r.URL.Query().Get("ip"))
		reply(w, v, e)
	})
	mux.HandleFunc("POST /v1/connect", func(w http.ResponseWriter, r *http.Request) {
		var v setup.Request
		if err := decode(r, &v); err != nil {
			reply(w, nil, err)
			return
		}
		reply(w, struct{}{}, api.Connect(r.Context(), v))
	})
	mux.HandleFunc("POST /v1/recover", func(w http.ResponseWriter, r *http.Request) {
		var v struct {
			Confirm bool `json:"confirm"`
		}
		if err := decode(r, &v); err != nil {
			reply(w, nil, err)
			return
		}
		if !v.Confirm {
			reply(w, nil, fmt.Errorf("explicit recovery confirmation required"))
			return
		}
		reply(w, struct{}{}, api.Recover(r.Context()))
	})
	mux.HandleFunc("POST /v1/retry", func(w http.ResponseWriter, r *http.Request) { reply(w, struct{}{}, api.Retry(r.Context())) })
	mux.HandleFunc("POST /v1/scan", func(w http.ResponseWriter, r *http.Request) { reply(w, struct{}{}, api.StartScan(r.Context())) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Body != nil && r.Body != http.NoBody {
			controller := http.NewResponseController(w)
			if err := controller.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				http.Error(w, "deadline unavailable", 500)
				return
			}
			data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16*1024))
			if err != nil {
				r.Close = true
				w.Header().Set("Connection", "close")
				controller.SetReadDeadline(time.Now())
				http.Error(w, "invalid request body", 400)
				return
			}
			r.Body.Close()
			controller.SetReadDeadline(time.Time{})
			r.Body = io.NopCloser(bytes.NewReader(data))
		}
		mux.ServeHTTP(w, r)
	})
}
func Listen(ctx context.Context, dir string, api service.API) (*Server, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		listener.Close()
		return nil, err
	}
	server := &Server{listener: listener, dir: dir, info: endpoint{Address: listener.Addr().String(), Token: hex.EncodeToString(token[:]), Protocol: 1}}
	data, err := json.Marshal(server.info)
	if err == nil {
		err = persist.WriteFile(endpointPath(dir), data)
	}
	if err != nil {
		listener.Close()
		return nil, err
	}
	server.server = &http.Server{Handler: handler(api, server.info.Token), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	return server, nil
}
func (s *Server) Serve() error { return s.server.Serve(s.listener) }
func (s *Server) Close() error {
	err := s.server.Close()
	removeErr := os.Remove(endpointPath(s.dir))
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(err, removeErr)
}

type Client struct {
	http *http.Client
	info endpoint
}

func Attach(ctx context.Context, dir string) (*Client, error) {
	data, err := os.ReadFile(endpointPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrUnavailable
	}
	if err != nil {
		return nil, err
	}
	var info endpoint
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(info.Address)
	if err != nil || host != "127.0.0.1" || info.Protocol != 1 || len(info.Token) != 64 {
		return nil, fmt.Errorf("invalid local service endpoint")
	}
	client := &Client{info: info, http: &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 2 * time.Minute}}
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := client.Status(probe); err != nil {
		client.Close()
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil, ErrUnavailable
		}
		return nil, err
	}
	return client, nil
}
func (c *Client) Close() { c.http.CloseIdleConnections() }
func (c *Client) call(ctx context.Context, method, path string, body, result any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://"+c.info.Address+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.info.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1024*1024))
	if response.StatusCode == http.StatusNotFound && path == "/v1/profile" {
		return fmt.Errorf("running receiver does not support scan profiles; update and restart it first")
	}
	if response.StatusCode != 200 {
		var f failure
		if err := decoder.Decode(&f); err != nil {
			return fmt.Errorf("control request failed: HTTP %d", response.StatusCode)
		}
		switch f.Code {
		case "busy":
			return service.ErrBusy
		case "conflict":
			return config.ErrConflict
		}
		return errors.New(f.Error)
	}
	return decoder.Decode(result)
}
func (c *Client) Status(ctx context.Context) (service.Status, error) {
	var v service.Status
	err := c.call(ctx, "GET", "/v1/status", nil, &v)
	return v, err
}
func (c *Client) Settings(ctx context.Context) (config.View, error) {
	var v config.View
	err := c.call(ctx, "GET", "/v1/settings", nil, &v)
	return v, err
}
func (c *Client) Save(ctx context.Context, u config.Update) (config.View, error) {
	var v config.View
	err := c.call(ctx, "PUT", "/v1/settings", u, &v)
	return v, err
}
func (c *Client) Discover(ctx context.Context, ip string) ([]setup.Candidate, error) {
	var v []setup.Candidate
	err := c.call(ctx, "GET", "/v1/discover?ip="+url.QueryEscape(ip), nil, &v)
	return v, err
}
func (c *Client) Connect(ctx context.Context, u setup.Request) error {
	var v any
	return c.call(ctx, "POST", "/v1/connect", u, &v)
}
func (c *Client) Recover(ctx context.Context) error {
	var v any
	return c.call(ctx, "POST", "/v1/recover", map[string]bool{"confirm": true}, &v)
}
func (c *Client) Retry(ctx context.Context) error {
	var v any
	return c.call(ctx, "POST", "/v1/retry", nil, &v)
}

func (c *Client) StartScan(ctx context.Context) error {
	var v any
	return c.call(ctx, "POST", "/v1/scan", nil, &v)
}

func (c *Client) Profile(ctx context.Context) (config.ProfileView, error) {
	var view config.ProfileView
	err := c.call(ctx, "GET", "/v1/profile", nil, &view)
	return view, err
}

func (c *Client) SaveProfile(ctx context.Context, update config.ProfileView) (config.ProfileView, error) {
	var view config.ProfileView
	err := c.call(ctx, "PUT", "/v1/profile", update, &view)
	return view, err
}
