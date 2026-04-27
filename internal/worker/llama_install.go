package worker

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// defaultLlamaVersion is the llama.cpp release tag the worker auto-installs
// when --llama-server is not provided. Bumped manually for now; future work
// can query the GitHub releases API.
const defaultLlamaVersion = "b8951"

// ensureLlamaServer returns a path to a llama-server binary, downloading
// and caching it under cacheDir if necessary. The whole release zip is
// extracted (we need the ggml-* dynamic libraries that ship alongside the
// binary), so cacheDir typically ends up containing llama-server[.exe]
// plus a handful of DLLs / shared libraries.
func ensureLlamaServer(ctx context.Context, cacheDir, version string) (string, error) {
	if version == "" {
		version = defaultLlamaVersion
	}
	binName := llamaServerBinaryName()
	target := filepath.Join(cacheDir, binName)

	if _, err := os.Stat(target); err == nil {
		log.Printf("llama-server cached: %s", target)
		return target, nil
	}

	asset, err := pickLlamaAsset(version)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("https://github.com/ggml-org/llama.cpp/releases/download/%s/%s", version, asset)

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	log.Printf("downloading llama.cpp %s (%s)", version, asset)
	if err := downloadAndExtractZip(ctx, url, cacheDir); err != nil {
		return "", fmt.Errorf("install llama-server: %w", err)
	}

	if _, err := os.Stat(target); err != nil {
		return "", fmt.Errorf("%s not found in extracted archive (looked at %s)", binName, target)
	}
	log.Printf("llama-server installed: %s", target)
	return target, nil
}

func llamaServerBinaryName() string {
	if runtime.GOOS == "windows" {
		return "llama-server.exe"
	}
	return "llama-server"
}

// pickLlamaAsset returns the release asset filename for the current
// platform. We default to the CPU build (works everywhere). Users who
// want CUDA/Vulkan/Metal acceleration should pass --llama-server PATH
// pointing at a manually-installed binary for now.
func pickLlamaAsset(version string) (string, error) {
	switch runtime.GOOS {
	case "windows":
		switch runtime.GOARCH {
		case "amd64":
			return fmt.Sprintf("llama-%s-bin-win-cpu-x64.zip", version), nil
		case "arm64":
			return fmt.Sprintf("llama-%s-bin-win-cpu-arm64.zip", version), nil
		}
	}
	return "", fmt.Errorf("auto-install of llama-server not supported on %s/%s yet — pass --llama-server PATH to a manually installed binary", runtime.GOOS, runtime.GOARCH)
}

func downloadAndExtractZip(ctx context.Context, url, destDir string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(buf))
	}

	tmp, err := os.CreateTemp(destDir, "llama-*.zip")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	n, err := io.Copy(tmp, resp.Body)
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	log.Printf("downloaded %s, extracting…", humanSize(n))

	zr, err := zip.OpenReader(tmpPath)
	if err != nil {
		return err
	}
	defer zr.Close()

	for _, f := range zr.File {
		if err := extractZipEntry(f, destDir); err != nil {
			return err
		}
	}
	return nil
}

func extractZipEntry(f *zip.File, destDir string) error {
	outPath := filepath.Join(destDir, f.Name)
	// guard against zip-slip
	rel, err := filepath.Rel(destDir, outPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("zip entry escapes dest: %s", f.Name)
	}
	if f.FileInfo().IsDir() {
		return os.MkdirAll(outPath, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	mode := f.Mode()
	if mode == 0 {
		mode = 0o644
	}
	out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}
