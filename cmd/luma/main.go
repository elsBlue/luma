package main

import (
	"context"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/font/gofont"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/text"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/elsBlue/luma/internal/adb"
	"github.com/elsBlue/luma/internal/mirror"
	"github.com/elsBlue/luma/internal/tools"
)

func main() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				alert("Luma", fmt.Sprintf("%v\n\n%s", r, debug.Stack()))
				os.Exit(1)
			}
		}()
		w := new(app.Window)
		w.Option(app.Title("Luma"), app.Size(1180, 760), app.MinSize(1020, 640))
		if err := run(w); err != nil {
			alert("Luma", err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}()
	app.Main()
}

type ui struct {
	win *app.Window
	th  *material.Theme

	mu       sync.Mutex
	bundle   tools.Bundle
	client   *adb.Client
	devices  []adb.Device
	selected string
	session  *mirror.Session
	status   string
	busy     bool
	quality  int
	full     bool

	refresh  widget.Clickable
	start    widget.Clickable
	shot     widget.Clickable
	record   widget.Clickable
	back     widget.Clickable
	home     widget.Clickable
	recents  widget.Clickable
	volUp    widget.Clickable
	volDown  widget.Clickable
	power    widget.Clickable
	rotate   widget.Clickable
	paste    widget.Clickable
	install  widget.Clickable
	send     widget.Clickable
	wireless widget.Clickable
	fullBtn  widget.Clickable
	quals    [4]widget.Clickable
	rows     map[string]*widget.Clickable
	ip       widget.Editor
	list     widget.List
	stage    stage
}

func run(w *app.Window) error {
	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))
	th.Palette = material.Palette{
		Fg:         color.NRGBA{R: 238, G: 242, B: 246, A: 255},
		Bg:         color.NRGBA{R: 18, G: 21, B: 27, A: 255},
		ContrastBg: color.NRGBA{R: 76, G: 141, B: 255, A: 255},
		ContrastFg: color.NRGBA{R: 6, G: 16, B: 28, A: 255},
	}
	u := &ui{
		win:     w,
		th:      th,
		status:  "Preparing Luma…",
		quality: 2,
		rows:    map[string]*widget.Clickable{},
	}
	u.ip.SingleLine = true
	u.ip.Submit = true
	u.list.Axis = layout.Vertical
	u.stage.ui = u
	go u.boot()

	var ops op.Ops
	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			u.stopSession()
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			u.layout(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

func (u *ui) boot() {
	exe, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	b, err := tools.Ensure(ctx, exe, func(msg string) {
		u.setStatus(msg)
	})
	if err != nil {
		u.setStatus(err.Error())
		return
	}
	u.mu.Lock()
	u.bundle = b
	u.client = &adb.Client{Path: b.ADB}
	u.mu.Unlock()
	cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
	_ = u.client.StartServer(cctx)
	ccancel()
	u.setStatus("Plug in the phone, unlock it, and allow USB debugging.")
	u.refreshDevices()
}

func (u *ui) setStatus(s string) {
	u.mu.Lock()
	u.status = s
	u.mu.Unlock()
	u.win.Invalidate()
}

func (u *ui) refreshDevices() {
	u.mu.Lock()
	c := u.client
	u.mu.Unlock()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	list, err := c.Devices(ctx)
	u.mu.Lock()
	defer u.mu.Unlock()
	if err != nil {
		u.status = err.Error()
		u.win.Invalidate()
		return
	}
	u.devices = list
	if u.selected == "" {
		for _, d := range list {
			if d.State == "device" {
				u.selected = d.Serial
				break
			}
		}
	}
	if u.session == nil {
		if len(list) == 0 {
			u.status = "No phone yet. Enable USB debugging, plug in, then Refresh."
		} else {
			u.status = "Select the phone and press Start."
		}
	}
	u.win.Invalidate()
}

func (u *ui) selectedDevice() (adb.Device, bool) {
	for _, d := range u.devices {
		if d.Serial == u.selected {
			return d, true
		}
	}
	return adb.Device{}, false
}

func (u *ui) stopSession() {
	u.mu.Lock()
	s := u.session
	u.session = nil
	u.mu.Unlock()
	if s != nil {
		s.Stop()
	}
}

func (u *ui) restartMirror() {
	u.mu.Lock()
	s := u.session
	u.session = nil
	u.busy = true
	u.status = "Switching picture…"
	u.mu.Unlock()
	u.win.Invalidate()
	if s != nil {
		s.Stop()
	}
	u.mu.Lock()
	u.busy = false
	u.mu.Unlock()
	u.beginMirror()
}

func (u *ui) beginMirror() {
	u.mu.Lock()
	if u.busy || u.client == nil {
		u.mu.Unlock()
		return
	}
	if u.session != nil {
		s := u.session
		u.session = nil
		u.busy = false
		u.status = "Stopped."
		u.mu.Unlock()
		go s.Stop()
		u.win.Invalidate()
		return
	}
	dev, ok := u.selectedDevice()
	if !ok || dev.State != "device" {
		u.status = "Choose a phone that says it is connected. If it says Allow, tap the prompt on the phone."
		u.mu.Unlock()
		u.win.Invalidate()
		return
	}
	u.busy = true
	u.status = "Starting mirror…"
	client := u.client
	bundle := u.bundle
	q := mirror.Qualities[u.quality]
	serial := dev.Serial
	u.mu.Unlock()
	u.win.Invalidate()
	go func() {
		sess, err := mirror.Start(context.Background(), client, serial, bundle.FFmpeg, bundle.Server, q, func() {
			u.win.Invalidate()
		})
		u.mu.Lock()
		u.busy = false
		if err != nil {
			u.status = err.Error()
			u.mu.Unlock()
			u.win.Invalidate()
			return
		}
		u.session = sess
		if sess.Warning != "" {
			u.status = sess.Warning
		} else if sess.Smooth() {
			u.status = "Live. Click the screen and type."
		} else {
			u.status = "Connected."
		}
		u.mu.Unlock()
		u.win.Invalidate()
	}()
}

func picturesDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "Pictures", "Luma")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func videosDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "Videos", "Luma")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func stamp() string { return time.Now().Format("20060102-150405") }
