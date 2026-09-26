package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

type instanceInfo struct {
	PID    int    `json:"pid"`
	Listen string `json:"listen"`
	URL    string `json:"url"`
	Config string `json:"config"`
}

type instanceLock struct {
	file *os.File
	path string
	info instanceInfo
}

func instanceLockPath() string {
	if p := os.Getenv("NEXAROUTE_INSTANCE_LOCK"); p != "" {
		return p
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "nexaroute", "instance.lock")
	}
	cfg := defaultConfigPath()
	return filepath.Join(filepath.Dir(cfg), "instance.lock")
}

func acquireInstanceLock(path string, info instanceInfo) (*instanceLock, *instanceInfo, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		existing := readInstanceInfo(f)
		_ = f.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, existing, nil
		}
		return nil, existing, err
	}
	payload, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, nil, err
	}
	if err := f.Truncate(0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, nil, err
	}
	if _, err := f.Write(append(payload, '\n')); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, nil, err
	}
	_ = f.Sync()
	return &instanceLock{file: f, path: path, info: info}, nil, nil
}

func readInstanceInfo(r io.Reader) *instanceInfo {
	var info instanceInfo
	if err := json.NewDecoder(r).Decode(&info); err != nil {
		return &instanceInfo{}
	}
	return &info
}

func (l *instanceLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}

func handleExistingInstance(existing *instanceInfo, out io.Writer) {
	if out == nil {
		out = os.Stderr
	}
	url := "http://127.0.0.1:8080/"
	if existing != nil && existing.URL != "" {
		url = existing.URL
	}
	fmt.Fprintf(out, "NexaRoute is already running")
	if existing != nil && existing.PID > 0 {
		fmt.Fprintf(out, " (pid %d)", existing.PID)
	}
	fmt.Fprintf(out, ".\nNot starting a second gateway.\n")
	openUI(url, out)
}
