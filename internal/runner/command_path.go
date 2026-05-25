package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func lookPathInEnv(file string, env []string) (string, error) {
	return lookPathInPATH(file, envPATH(env))
}

func envPATH(env []string) string {
	for _, envPair := range env {
		name, value, ok := strings.Cut(envPair, "=")
		if ok && name == "PATH" {
			return value
		}
	}
	return ""
}

func lookPathInPATH(file, pathValue string) (string, error) {
	if strings.ContainsAny(file, `/\`) || pathValue == "" {
		return exec.LookPath(file)
	}
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			dir = "."
		}
		path := filepath.Join(dir, file)
		if err := executableFile(path); err == nil {
			return path, nil
		}
	}
	return "", &exec.Error{Name: file, Err: exec.ErrNotFound}
}

func executableFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return exec.ErrNotFound
	}
	if runtime.GOOS == "windows" || info.Mode()&0o111 != 0 {
		return nil
	}
	return os.ErrPermission
}
