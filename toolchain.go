// Package toolchainext provides the toolchain.install capability for Pulp
// cells: download + extract a language toolchain into the host's bundled
// runtime dir. A sandboxed WASM cell can't write the host's runtime dir
// itself (it only has its own scoped storage), so the host fetches and
// unpacks the toolchain here, into the same on-disk layout the host's tool
// resolver (tools.go) looks for: runtime/go/bin/go(.exe), runtime/git/cmd/git.exe.
//
// This is the piece that lets a distributable build acquire a build toolchain
// on demand — the provisioner asks for "go" and the host materializes it under
// <exeDir>/runtime/go, picked up automatically on the next tool resolve.
//
// Deployment:
//
//	import _ "github.com/BananaLabs-OSS/Pulp-ext-toolchain"
//
// Host imports:
//
//	toolchain_install(req_ptr, req_len, resp_ptr_out, resp_len_out) -> code  # req{lang}; resp{ok,status,message}
//	toolchain_status(req_ptr, req_len, resp_ptr_out, resp_len_out)  -> code  # req{lang}; resp{present,path}
package toolchainext

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/vmihailenco/msgpack/v5"
)

const (
	codeOK            = 0
	codeMemRead       = 2
	codeDecode        = 3
	codeFetchFailed   = 4
	codeExtractFailed = 5
	codeAllocFailed   = 7
	codeMemWrite      = 8
	codeCapAbsent     = 99
)

var logger = slog.Default()

// toolEntry describes how to fetch + lay out one toolchain for one os/arch.
type toolEntry struct {
	url        string // download URL (zip or tar.gz, inferred from suffix)
	binRelPath string // expected binary path relative to subdir, e.g. "bin/go.exe"
}

// toolDef groups the per-os/arch entries for a language plus its on-disk
// subdir under runtime/ (go→"go", git→"git"), matching tools.go.
type toolDef struct {
	subdir  string               // dir under runtime/, e.g. "go" → runtime/go
	entries map[string]toolEntry // key "<os>/<arch>"
}

// toolTable is the pinned URL table. Pins mirror make-runtime.ps1 (Go 1.25.6,
// MinGit 2.47.1). Keyed lang → {os/arch → entry}. Unknown lang/os → unsupported.
var toolTable = map[string]toolDef{
	"go": {
		subdir: "go",
		entries: map[string]toolEntry{
			"windows/amd64": {url: "https://go.dev/dl/go1.25.6.windows-amd64.zip", binRelPath: "bin/go.exe"},
			"linux/amd64":   {url: "https://go.dev/dl/go1.25.6.linux-amd64.tar.gz", binRelPath: "bin/go"},
			"linux/arm64":   {url: "https://go.dev/dl/go1.25.6.linux-arm64.tar.gz", binRelPath: "bin/go"},
			"darwin/amd64":  {url: "https://go.dev/dl/go1.25.6.darwin-amd64.tar.gz", binRelPath: "bin/go"},
			"darwin/arm64":  {url: "https://go.dev/dl/go1.25.6.darwin-arm64.tar.gz", binRelPath: "bin/go"},
		},
	},
	"git": {
		subdir: "git",
		entries: map[string]toolEntry{
			// MinGit = the minimal Git-for-Windows (no GUI). Windows-only; on other
			// platforms git is expected from the system PATH.
			"windows/amd64": {url: "https://github.com/git-for-windows/git/releases/download/v2.47.1.windows.1/MinGit-2.47.1-64-bit.zip", binRelPath: "cmd/git.exe"},
		},
	},
}

func init() {
	ext.Register(ext.Capability{
		Name: "toolchain.install",
		Setup: func(env ext.SetupEnv) error {
			if env.Logger != nil {
				logger = env.Logger
			}
			return nil
		},
		Register: bindActive,
		Stub:     bindStub,
	})
}

func bindActive(b wazero.HostModuleBuilder, _ ext.Cell) error {
	b.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, reqPtr, reqLen, respPtrOut, respLenOut uint32) uint32 {
		return toolchainInstall(ctx, m, reqPtr, reqLen, respPtrOut, respLenOut)
	}).Export("toolchain_install")
	b.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, reqPtr, reqLen, respPtrOut, respLenOut uint32) uint32 {
		return toolchainStatus(ctx, m, reqPtr, reqLen, respPtrOut, respLenOut)
	}).Export("toolchain_status")
	return nil
}

func bindStub(b wazero.HostModuleBuilder, _ ext.Cell) error {
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, _ api.Module, _, _, _, _ uint32) uint32 { return codeCapAbsent }).Export("toolchain_install")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, _ api.Module, _, _, _, _ uint32) uint32 { return codeCapAbsent }).Export("toolchain_status")
	return nil
}

// runtimeDir is <dir-of-host-exe>/runtime — where a self-contained build
// carries its own toolchain (mirrors tools.go bundledRuntimeDir).
func runtimeDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), "runtime"), nil
}

func toolchainInstall(ctx context.Context, m api.Module, reqPtr, reqLen, respPtrOut, respLenOut uint32) uint32 {
	var req struct {
		Lang string `msgpack:"lang"`
	}
	if reqLen > 0 {
		data, ok := m.Memory().Read(reqPtr, reqLen)
		if !ok {
			return codeMemRead
		}
		if err := msgpack.Unmarshal(data, &req); err != nil {
			return codeDecode
		}
	}

	def, ok := toolTable[req.Lang]
	if !ok {
		return reply(ctx, m, respPtrOut, respLenOut, installResp{Ok: false, Status: "unsupported", Message: fmt.Sprintf("unknown lang %q", req.Lang)})
	}
	key := runtime.GOOS + "/" + runtime.GOARCH
	entry, ok := def.entries[key]
	if !ok {
		return reply(ctx, m, respPtrOut, respLenOut, installResp{Ok: false, Status: "unsupported", Message: fmt.Sprintf("%s not supported on %s", req.Lang, key)})
	}

	rt, err := runtimeDir()
	if err != nil {
		return reply(ctx, m, respPtrOut, respLenOut, installResp{Ok: false, Status: "failed", Message: "resolve runtime dir: " + err.Error()})
	}
	if err := os.MkdirAll(rt, 0o755); err != nil {
		return reply(ctx, m, respPtrOut, respLenOut, installResp{Ok: false, Status: "failed", Message: "create runtime dir: " + err.Error()})
	}
	dest := filepath.Join(rt, def.subdir)

	// Already present? short-circuit (idempotent, like make-runtime.ps1).
	binAbs := filepath.Join(dest, filepath.FromSlash(entry.binRelPath))
	if fi, statErr := os.Stat(binAbs); statErr == nil && !fi.IsDir() {
		return reply(ctx, m, respPtrOut, respLenOut, installResp{Ok: true, Status: "present", Message: binAbs})
	}

	tmp, err := download(entry.url)
	if err != nil {
		logger.Error("toolchain download", "lang", req.Lang, "url", entry.url, "err", err)
		return reply(ctx, m, respPtrOut, respLenOut, installResp{Ok: false, Status: "failed", Message: "download: " + err.Error()})
	}
	defer os.Remove(tmp)

	// Fresh extract: clear any partial dir first.
	_ = os.RemoveAll(dest)
	if err := extract(tmp, dest, def.subdir); err != nil {
		logger.Error("toolchain extract", "lang", req.Lang, "err", err)
		return reply(ctx, m, respPtrOut, respLenOut, installResp{Ok: false, Status: "failed", Message: "extract: " + err.Error()})
	}

	if fi, statErr := os.Stat(binAbs); statErr != nil || fi.IsDir() {
		return reply(ctx, m, respPtrOut, respLenOut, installResp{Ok: false, Status: "failed", Message: "binary missing after extract: " + binAbs})
	}
	logger.Info("toolchain installed", "lang", req.Lang, "path", binAbs)
	return reply(ctx, m, respPtrOut, respLenOut, installResp{Ok: true, Status: "present", Message: binAbs})
}

func toolchainStatus(ctx context.Context, m api.Module, reqPtr, reqLen, respPtrOut, respLenOut uint32) uint32 {
	var req struct {
		Lang string `msgpack:"lang"`
	}
	if reqLen > 0 {
		data, ok := m.Memory().Read(reqPtr, reqLen)
		if !ok {
			return codeMemRead
		}
		if err := msgpack.Unmarshal(data, &req); err != nil {
			return codeDecode
		}
	}

	def, ok := toolTable[req.Lang]
	if !ok {
		return reply(ctx, m, respPtrOut, respLenOut, statusResp{Present: false, Path: ""})
	}
	// Pick the entry for this os/arch to know the expected binRelPath; fall
	// back to any entry so we can still probe the subdir on unsupported os.
	entry, ok := def.entries[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		for _, e := range def.entries {
			entry = e
			break
		}
	}
	rt, err := runtimeDir()
	if err != nil {
		return reply(ctx, m, respPtrOut, respLenOut, statusResp{Present: false, Path: ""})
	}
	binAbs := filepath.Join(rt, def.subdir, filepath.FromSlash(entry.binRelPath))
	if fi, statErr := os.Stat(binAbs); statErr == nil && !fi.IsDir() {
		return reply(ctx, m, respPtrOut, respLenOut, statusResp{Present: true, Path: binAbs})
	}
	return reply(ctx, m, respPtrOut, respLenOut, statusResp{Present: false, Path: ""})
}

type installResp struct {
	Ok      bool   `msgpack:"ok"`
	Status  string `msgpack:"status"`
	Message string `msgpack:"message"`
}

type statusResp struct {
	Present bool   `msgpack:"present"`
	Path    string `msgpack:"path"`
}

// reply marshals v and writes it back into cell memory; returns codeOK or an
// alloc/write failure code.
func reply(ctx context.Context, m api.Module, respPtrOut, respLenOut uint32, v any) uint32 {
	payload, err := msgpack.Marshal(v)
	if err != nil {
		return codeAllocFailed
	}
	return writeResp(ctx, m, payload, respPtrOut, respLenOut)
}

// download fetches url into a temp file and returns its path. Caller removes it.
func download(url string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %d for %s", resp.StatusCode, url)
	}
	suffix := ".zip"
	if strings.HasSuffix(url, ".tar.gz") || strings.HasSuffix(url, ".tgz") {
		suffix = ".tar.gz"
	}
	f, err := os.CreateTemp("", "toolchain-*"+suffix)
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(f.Name())
		return "", copyErr
	}
	if closeErr != nil {
		os.Remove(f.Name())
		return "", closeErr
	}
	return f.Name(), nil
}

// extract unpacks the archive at src into dest, flattening a single wrapping
// top-level dir whose name equals subdir (the Go zip's root is "go/", so
// "go/bin/go.exe" lands at "<dest>/bin/go.exe"). Other archives (MinGit, whose
// root is cmd/, mingw64/ …) are unpacked verbatim. Archive type is inferred
// from src's suffix.
func extract(src, dest, subdir string) error {
	if strings.HasSuffix(src, ".tar.gz") || strings.HasSuffix(src, ".tgz") {
		return extractTarGz(src, dest, subdir)
	}
	return extractZip(src, dest, subdir)
}

// flatten strips a leading "<subdir>/" component from name if present, so a
// single wrapping top dir collapses into dest. Returns "" to skip the bare
// wrapping dir entry itself.
func flatten(name, subdir string) string {
	name = strings.TrimPrefix(filepath.ToSlash(name), "./")
	prefix := subdir + "/"
	if name == subdir || name == prefix {
		return ""
	}
	if strings.HasPrefix(name, prefix) {
		return strings.TrimPrefix(name, prefix)
	}
	return name
}

// safeJoin joins dest+rel rejecting traversal outside dest (zip-slip guard).
func safeJoin(dest, rel string) (string, error) {
	target := filepath.Join(dest, filepath.FromSlash(rel))
	cleanDest := filepath.Clean(dest)
	if target != cleanDest && !strings.HasPrefix(target, cleanDest+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe path %q", rel)
	}
	return target, nil
}

func extractZip(src, dest, subdir string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		rel := flatten(zf.Name, subdir)
		if rel == "" {
			continue
		}
		target, err := safeJoin(dest, rel)
		if err != nil {
			return err
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		mode := zf.Mode()
		if mode == 0 {
			mode = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
		if err != nil {
			rc.Close()
			return err
		}
		_, copyErr := io.Copy(out, rc)
		rc.Close()
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func extractTarGz(src, dest, subdir string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		rel := flatten(hdr.Name, subdir)
		if rel == "" {
			continue
		}
		target, err := safeJoin(dest, rel)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode).Perm()
			if mode == 0 {
				mode = 0o644
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink, tar.TypeLink:
			// Skip links — Go's tarball has none in the bin path we verify;
			// avoids cross-platform symlink + traversal headaches.
			continue
		}
	}
	return nil
}

func writeResp(ctx context.Context, m api.Module, data []byte, respPtrOut, respLenOut uint32) uint32 {
	allocFn := m.ExportedFunction("pulp_alloc")
	if allocFn == nil {
		return codeAllocFailed
	}
	res, err := allocFn.Call(ctx, uint64(len(data)))
	if err != nil || len(res) == 0 {
		return codeAllocFailed
	}
	ptr := uint32(res[0])
	if ptr == 0 || !m.Memory().Write(ptr, data) {
		return codeMemWrite
	}
	if !m.Memory().WriteUint32Le(respPtrOut, ptr) || !m.Memory().WriteUint32Le(respLenOut, uint32(len(data))) {
		return codeMemWrite
	}
	return codeOK
}
