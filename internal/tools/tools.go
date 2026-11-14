package tools

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ServerURL = "https://github.com/Genymobile/scrcpy/releases/download/v5.0.1/scrcpy-server-v5.0.1"
	ADBURL    = "https://dl.google.com/android/repository/platform-tools-latest-windows.zip"
	FFmpegURL = "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-lgpl.zip"
)

type Bundle struct {
	Dir    string
	ADB    string
	FFmpeg string
	Server string
}

func DirBesideExe(exe string) string {
	return filepath.Join(filepath.Dir(exe), "tools")
}

func CacheDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(base, "Luma", "tools")
}

func Ready(dir string) (Bundle, bool) {
	b := Bundle{
		Dir:    dir,
		ADB:    filepath.Join(dir, "adb.exe"),
		FFmpeg: filepath.Join(dir, "ffmpeg.exe"),
		Server: filepath.Join(dir, "luma-server"),
	}
	if fileOK(b.ADB) && fileOK(b.FFmpeg) && fileOK(b.Server) {
		return b, true
	}
	return b, false
}

func fileOK(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Size() > 0 && !st.IsDir()
}

func Ensure(ctx context.Context, exePath string, progress func(string)) (Bundle, error) {
	if progress == nil {
		progress = func(string) {}
	}
	beside := DirBesideExe(exePath)
	if b, ok := Ready(beside); ok {
		progress("Tools ready")
		return b, nil
	}
	dir := CacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Bundle{}, err
	}
	if b, ok := Ready(dir); ok {
		progress("Tools ready")
		return b, nil
	}
	b := Bundle{
		Dir:    dir,
		ADB:    filepath.Join(dir, "adb.exe"),
		FFmpeg: filepath.Join(dir, "ffmpeg.exe"),
		Server: filepath.Join(dir, "luma-server"),
	}
	if !fileOK(b.Server) {
		progress("Downloading phone service…")
		if err := download(ctx, ServerURL, b.Server, progress); err != nil {
			return Bundle{}, fmt.Errorf("phone service: %w", err)
		}
	}
	if !fileOK(b.ADB) {
		progress("Downloading USB bridge…")
		if err := downloadZip(ctx, ADBURL, dir, "adb.exe", progress); err != nil {
			return Bundle{}, fmt.Errorf("USB bridge: %w", err)
		}
	}
	if !fileOK(b.FFmpeg) {
		progress("Downloading video decoder…")
		if err := downloadZip(ctx, FFmpegURL, dir, "ffmpeg.exe", progress); err != nil {
			return Bundle{}, fmt.Errorf("video decoder: %w", err)
		}
	}
	if _, ok := Ready(dir); !ok {
		return Bundle{}, fmt.Errorf("tools did not unpack correctly")
	}
	progress("Tools ready")
	return b, nil
}

func download(ctx context.Context, url, dest string, progress func(string)) error {
	tmp := dest + ".part"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Luma/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download %s returned %s", url, resp.Status)
	}
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	buf := make([]byte, 256*1024)
	var got int64
	last := time.Now()
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return werr
			}
			got += int64(n)
			if time.Since(last) > 400*time.Millisecond {
				if resp.ContentLength > 0 {
					progress(fmt.Sprintf("Downloading %d%%", got*100/resp.ContentLength))
				} else {
					progress(fmt.Sprintf("Downloading %d MB", got/1024/1024))
				}
				last = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

func downloadZip(ctx context.Context, url, destDir, wanted string, progress func(string)) error {
	tmp := filepath.Join(destDir, wanted+".zip")
	if err := download(ctx, url, tmp, progress); err != nil {
		return err
	}
	defer os.Remove(tmp)
	progress("Unpacking…")
	return extractWanted(tmp, destDir, wanted)
}

func extractWanted(zipPath, destDir, wanted string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	var hit *zip.File
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Base(f.Name), wanted) {
			hit = f
			break
		}
	}
	if hit == nil {
		return fmt.Errorf("archive has no %s", wanted)
	}
	dir := filepath.ToSlash(filepath.Dir(hit.Name))
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := filepath.Base(f.Name)
		parent := filepath.ToSlash(filepath.Dir(f.Name))
		if parent != dir {
			continue
		}
		low := strings.ToLower(name)
		if !(strings.EqualFold(name, wanted) || strings.HasSuffix(low, ".dll")) {
			continue
		}
		if err := extractFile(f, filepath.Join(destDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func extractFile(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, rc)
	cerr := out.Close()
	if err != nil {
		return err
	}
	return cerr
}
