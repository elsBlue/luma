package main

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"time"

	"gioui.org/app"
	"gioui.org/f32"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/elsBlue/luma/internal/mirror"
	"github.com/elsBlue/luma/internal/proto"
)

var (
	colBg    = color.NRGBA{R: 11, G: 13, B: 17, A: 255}
	colPanel = color.NRGBA{R: 18, G: 21, B: 27, A: 255}
	colRaise = color.NRGBA{R: 27, G: 33, B: 42, A: 255}
	colLine  = color.NRGBA{R: 44, G: 52, B: 64, A: 255}
	colText  = color.NRGBA{R: 238, G: 242, B: 246, A: 255}
	colMute  = color.NRGBA{R: 147, G: 160, B: 176, A: 255}
	colAmber = color.NRGBA{R: 76, G: 141, B: 255, A: 255}
	colInk   = color.NRGBA{R: 6, G: 16, B: 28, A: 255}
	colRec   = color.NRGBA{R: 255, G: 77, B: 77, A: 255}
)

type stage struct {
	ui   *ui
	geom box
}

type box struct {
	offX, offY int
	scale      float32
	imgW, imgH int
	vidW, vidH int
	down       bool
	sx, sy     int
	lx, ly     int
}

func (u *ui) layout(gtx layout.Context) layout.Dimensions {
	paint.Fill(gtx.Ops, colBg)
	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Dp(unit.Dp(320))
			gtx.Constraints.Max.X = gtx.Constraints.Min.X
			gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
			return u.sidebar(gtx)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return u.stage.layout(gtx)
		}),
	)
}

func (u *ui) sidebar(gtx layout.Context) layout.Dimensions {
	paint.FillShape(gtx.Ops, colPanel, clip.Rect{Max: gtx.Constraints.Max}.Op())
	u.act(gtx)
	return layout.UniformInset(unit.Dp(16)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return u.list.Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
			return u.sideBody(gtx)
		})
	})
}

func (u *ui) act(gtx layout.Context) {
	if u.refresh.Clicked(gtx) {
		go u.refreshDevices()
	}
	if u.start.Clicked(gtx) {
		u.beginMirror()
	}
	if u.shot.Clicked(gtx) {
		u.saveShot()
	}
	if u.record.Clicked(gtx) {
		u.toggleRecord()
	}
	for i := range u.quals {
		if u.quals[i].Clicked(gtx) {
			u.mu.Lock()
			changed := u.quality != i
			u.quality = i
			live := u.session != nil && changed
			u.mu.Unlock()
			if live {
				go u.restartMirror()
			}
		}
	}
	u.mu.Lock()
	rows := append([]string(nil), u.deviceSerials()...)
	u.mu.Unlock()
	for _, serial := range rows {
		if u.row(serial).Clicked(gtx) {
			u.mu.Lock()
			u.selected = serial
			u.mu.Unlock()
		}
	}
	if u.back.Clicked(gtx) {
		u.withSession(func(s *mirror.Session) { s.Back() })
	}
	if u.home.Clicked(gtx) {
		u.withSession(func(s *mirror.Session) { s.Key(proto.KeyHome) })
	}
	if u.recents.Clicked(gtx) {
		u.withSession(func(s *mirror.Session) { s.Key(proto.KeyAppSwitch) })
	}
	if u.volDown.Clicked(gtx) {
		u.withSession(func(s *mirror.Session) { s.Key(proto.KeyVolumeDown) })
	}
	if u.volUp.Clicked(gtx) {
		u.withSession(func(s *mirror.Session) { s.Key(proto.KeyVolumeUp) })
	}
	if u.power.Clicked(gtx) {
		u.withSession(func(s *mirror.Session) { s.Key(proto.KeyPower) })
	}
	if u.rotate.Clicked(gtx) {
		u.withSession(func(s *mirror.Session) { s.Rotate() })
	}
	if u.paste.Clicked(gtx) {
		go func() {
			text, err := clipText()
			if err != nil || text == "" {
				u.setStatus("Clipboard is empty.")
				return
			}
			u.withSession(func(s *mirror.Session) { s.Paste(text) })
			u.setStatus("Pasted onto the phone.")
		}()
	}
	if u.install.Clicked(gtx) {
		go u.installAPK()
	}
	if u.send.Clicked(gtx) {
		go u.sendFile()
	}
	if u.wireless.Clicked(gtx) {
		go u.connectWireless()
	}
	if u.fullBtn.Clicked(gtx) {
		u.mu.Lock()
		u.full = !u.full
		full := u.full
		u.mu.Unlock()
		if full {
			u.win.Option(app.Fullscreen.Option())
		} else {
			u.win.Option(app.Windowed.Option())
		}
	}
}

func (u *ui) sideBody(gtx layout.Context) layout.Dimensions {
	u.mu.Lock()
	status := u.status
	busy := u.busy
	live := u.session != nil
	q := u.quality
	devs := u.labels()
	selected := u.selected
	sess := u.session
	recording := sess != nil && sess.Recording()
	u.mu.Unlock()
	trace := ""
	logPath := ""
	if sess != nil {
		trace, logPath = sess.TraceLine()
	}

	startLabel := "Start"
	if live {
		startLabel = "Stop"
	} else if busy {
		startLabel = "Starting…"
	}
	recLabel := "Record"
	if recording {
		recLabel = "Stop recording"
	}

	kids := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			l := material.H5(u.th, "Luma")
			l.Color = colAmber
			return l.Layout(gtx)
		}),
		rigid(4),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			l := material.Label(u.th, unit.Sp(13), "Mirror a phone. USB stays low-delay. Picture never goes past 1080p.")
			l.Color = colMute
			return l.Layout(gtx)
		}),
		rigid(16),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.btn(gtx, &u.refresh, "Refresh phones", colRaise, colText)
		}),
		rigid(8),
	}
	if len(devs) == 0 {
		kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			l := material.Label(u.th, unit.Sp(13), "No phone detected.")
			l.Color = colMute
			return l.Layout(gtx)
		}))
	}
	for _, d := range devs {
		d := d
		kids = append(kids, rigid(6), layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			bg, fg := colRaise, colText
			if d.serial == selected {
				bg, fg = colAmber, colInk
			}
			return u.btn(gtx, u.row(d.serial), d.label, bg, fg)
		}))
	}
	kids = append(kids,
		rigid(14),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.btn(gtx, &u.start, startLabel, colAmber, colInk)
		}),
		rigid(10),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			l := material.Label(u.th, unit.Sp(12), "Picture · 1080p cap")
			l.Color = colMute
			return l.Layout(gtx)
		}),
		rigid(6),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
				u.qbtn(0, q), u.qbtn(1, q), u.qbtn(2, q), u.qbtn(3, q),
			)
		}),
		rigid(4),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			l := material.Label(u.th, unit.Sp(12), "Fast is the quickest. YouTube is 1080p60 for a clean recording.")
			l.Color = colMute
			return l.Layout(gtx)
		}),
		rigid(12),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return row3(gtx, u, &u.back, "Back", &u.home, "Home", &u.recents, "Recents")
		}),
		rigid(6),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return row3(gtx, u, &u.volDown, "Vol −", &u.volUp, "Vol +", &u.power, "Power")
		}),
		rigid(6),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return row2(gtx, u, &u.rotate, "Rotate", &u.paste, "Paste")
		}),
		rigid(6),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			bg := colRaise
			if recLabel != "Record" {
				bg = colRec
			}
			return row2c(gtx, u, &u.shot, "Screenshot", colRaise, &u.record, recLabel, bg)
		}),
		rigid(12),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			l := material.Label(u.th, unit.Sp(12), "Wi-Fi address, if you want wireless")
			l.Color = colMute
			return l.Layout(gtx)
		}),
		rigid(4),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return editorBox(gtx, u.th, &u.ip, "192.168.1.20")
		}),
		rigid(6),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.btn(gtx, &u.wireless, "Connect wireless", colRaise, colText)
		}),
		rigid(8),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return row2(gtx, u, &u.install, "Install APK", &u.send, "Send file")
		}),
		rigid(8),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.btn(gtx, &u.fullBtn, "Full screen", colRaise, colText)
		}),
		rigid(14),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			l := material.Label(u.th, unit.Sp(13), status)
			l.Color = colText
			return l.Layout(gtx)
		}),
	)
	if trace != "" {
		kids = append(kids, rigid(6), layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			l := material.Label(u.th, unit.Sp(12), trace)
			l.Color = colMute
			return l.Layout(gtx)
		}))
	}
	if logPath != "" {
		kids = append(kids, rigid(4), layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			l := material.Label(u.th, unit.Sp(11), logPath)
			l.Color = colMute
			return l.Layout(gtx)
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}

func (s *stage) layout(gtx layout.Context) layout.Dimensions {
	size := gtx.Constraints.Max
	defer clip.Rect{Max: size}.Push(gtx.Ops).Pop()
	event.Op(gtx.Ops, s)
	s.handle(gtx)
	paint.FillShape(gtx.Ops, colBg, clip.Rect{Max: size}.Op())

	u := s.ui
	u.mu.Lock()
	sess := u.session
	u.mu.Unlock()
	var img image.Image
	vw, vh := 0, 0
	if sess != nil {
		fr := sess.Frame()
		img = fr.Image
		vw, vh = fr.VideoW, fr.VideoH
	}
	if img == nil {
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			l := material.Label(u.th, unit.Sp(16), "The phone screen shows up here.")
			l.Color = colMute
			return l.Layout(gtx)
		})
	}
	b := img.Bounds()
	iw, ih := b.Dx(), b.Dy()
	if iw < 1 || ih < 1 {
		return layout.Dimensions{Size: size}
	}
	scale := float32(size.X) / float32(iw)
	if sy := float32(size.Y) / float32(ih); sy < scale {
		scale = sy
	}
	dw, dh := int(float32(iw)*scale), int(float32(ih)*scale)
	offX := (size.X - dw) / 2
	offY := (size.Y - dh) / 2
	s.geom = box{offX: offX, offY: offY, scale: scale, imgW: iw, imgH: ih, vidW: vw, vidH: vh}
	if s.geom.vidW <= 0 {
		s.geom.vidW, s.geom.vidH = iw, ih
	}

	stack := op.Offset(image.Pt(offX, offY)).Push(gtx.Ops)
	aff := op.Affine(f32.Affine2D{}.Scale(f32.Point{}, f32.Pt(scale, scale))).Push(gtx.Ops)
	// Nearest skips the mipmap chain. Building one for every phone frame
	// is what made a smooth stream look stepped.
	imgOp := paint.NewImageOp(img)
	imgOp.Filter = paint.FilterNearest
	imgOp.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	aff.Pop()
	stack.Pop()
	return layout.Dimensions{Size: size}
}

func (s *stage) handle(gtx layout.Context) {
	for {
		ev, ok := gtx.Event(pointer.Filter{
			Target:  s,
			Kinds:   pointer.Press | pointer.Release | pointer.Drag | pointer.Scroll | pointer.Cancel,
			ScrollX: pointer.ScrollRange{Min: -800, Max: 800},
			ScrollY: pointer.ScrollRange{Min: -800, Max: 800},
		})
		if !ok {
			break
		}
		e, ok := ev.(pointer.Event)
		if !ok {
			continue
		}
		switch e.Kind {
		case pointer.Press:
			gtx.Execute(key.FocusCmd{Tag: s})
			if e.Buttons.Contain(pointer.ButtonSecondary) {
				s.ui.withSession(func(sess *mirror.Session) { sess.Back() })
				continue
			}
			gtx.Execute(pointer.GrabCmd{Tag: s, ID: e.PointerID})
			x, y, ok := s.mapPoint(e.Position)
			if !ok {
				continue
			}
			s.geom.down = true
			s.geom.sx, s.geom.sy = x, y
			s.geom.lx, s.geom.ly = x, y
			s.touch(proto.ActionDown, x, y)
		case pointer.Drag:
			if !s.geom.down {
				continue
			}
			x, y, ok := s.mapPoint(e.Position)
			if !ok {
				continue
			}
			s.geom.lx, s.geom.ly = x, y
			s.touch(proto.ActionMove, x, y)
		case pointer.Release, pointer.Cancel:
			if !s.geom.down {
				continue
			}
			x, y, ok := s.mapPoint(e.Position)
			if !ok {
				x, y = s.geom.lx, s.geom.ly
			}
			s.geom.down = false
			dx, dy := x-s.geom.sx, y-s.geom.sy
			if dx < 0 {
				dx = -dx
			}
			if dy < 0 {
				dy = -dy
			}
			s.ui.mu.Lock()
			sess := s.ui.session
			s.ui.mu.Unlock()
			if sess != nil && !sess.Smooth() && (dx > 12 || dy > 12) {
				w, h := s.deviceSize()
				sess.Swipe(s.geom.sx, s.geom.sy, x, y, w, h)
			} else {
				s.touch(proto.ActionUp, x, y)
			}
		case pointer.Scroll:
			x, y, ok := s.mapPoint(e.Position)
			if !ok {
				continue
			}
			steps := e.Scroll.Y
			if steps > 8 || steps < -8 {
				steps = steps / 120
			}
			if steps == 0 {
				if e.Scroll.Y > 0 {
					steps = 1
				} else if e.Scroll.Y < 0 {
					steps = -1
				}
			}
			w, h := s.deviceSize()
			s.ui.withSession(func(sess *mirror.Session) {
				sess.Scroll(x, y, w, h, float32(-steps))
			})
		}
	}
	opts := key.ModShift | key.ModCtrl | key.ModAlt | key.ModSuper | key.ModCommand
	for {
		ev, ok := gtx.Event(key.Filter{Focus: s, Optional: opts})
		if !ok {
			break
		}
		e, ok := ev.(key.Event)
		if !ok || e.State != key.Press {
			continue
		}
		if e.Modifiers.Contain(key.ModCtrl) || e.Modifiers.Contain(key.ModSuper) || e.Modifiers.Contain(key.ModAlt) {
			continue
		}
		s.key(e)
	}
}

func (s *stage) key(e key.Event) {
	special := map[key.Name]uint32{
		key.NameEscape:         proto.KeyEscape,
		key.NameReturn:         proto.KeyEnter,
		key.NameEnter:          proto.KeyEnter,
		key.NameDeleteBackward: proto.KeyDel,
		key.NameDeleteForward:  proto.KeyForwardDel,
		key.NameTab:            proto.KeyTab,
		key.NameUpArrow:        proto.KeyDpadUp,
		key.NameDownArrow:      proto.KeyDpadDown,
		key.NameLeftArrow:      proto.KeyDpadLeft,
		key.NameRightArrow:     proto.KeyDpadRight,
		key.NameHome:           proto.KeyMoveHome,
		key.NameEnd:            proto.KeyMoveEnd,
		key.NamePageUp:         proto.KeyPageUp,
		key.NamePageDown:       proto.KeyPageDown,
		key.NameSpace:          proto.KeySpace,
		key.NameBack:           proto.KeyBack,
	}
	if code, ok := special[e.Name]; ok {
		if code == proto.KeyEscape {
			s.ui.withSession(func(sess *mirror.Session) { sess.Back() })
			return
		}
		s.ui.withSession(func(sess *mirror.Session) { sess.Key(code) })
		return
	}
	name := string(e.Name)
	if len([]rune(name)) != 1 {
		return
	}
	r := []rune(name)[0]
	if r >= 'A' && r <= 'Z' && !e.Modifiers.Contain(key.ModShift) {
		r = r - 'A' + 'a'
	}
	s.ui.withSession(func(sess *mirror.Session) { sess.Text(string(r)) })
}

func (s *stage) mapPoint(p f32.Point) (int, int, bool) {
	g := s.geom
	if g.scale <= 0 {
		return 0, 0, false
	}
	x := int((p.X - float32(g.offX)) / g.scale)
	y := int((p.Y - float32(g.offY)) / g.scale)
	if x < 0 || y < 0 || x >= g.imgW || y >= g.imgH {
		return 0, 0, false
	}
	if g.vidW > 0 && g.imgW > 0 {
		x = x * g.vidW / g.imgW
		y = y * g.vidH / g.imgH
	}
	return x, y, true
}

func (s *stage) deviceSize() (int, int) {
	if s.geom.vidW > 0 {
		return s.geom.vidW, s.geom.vidH
	}
	return s.geom.imgW, s.geom.imgH
}

func (s *stage) touch(action byte, x, y int) {
	w, h := s.deviceSize()
	s.ui.withSession(func(sess *mirror.Session) {
		if action == proto.ActionMove && !sess.Smooth() {
			return
		}
		sess.Touch(action, x, y, w, h)
	})
}

func (u *ui) withSession(fn func(*mirror.Session)) {
	u.mu.Lock()
	s := u.session
	u.mu.Unlock()
	if s != nil {
		fn(s)
	}
}

func (u *ui) saveShot() {
	u.mu.Lock()
	s := u.session
	u.mu.Unlock()
	if s == nil {
		u.setStatus("Start the mirror before taking a screenshot.")
		return
	}
	img := s.Frame().Image
	if img == nil {
		u.setStatus("No picture yet.")
		return
	}
	path := filepath.Join(picturesDir(), "luma-"+stamp()+".png")
	f, err := os.Create(path)
	if err != nil {
		u.setStatus(err.Error())
		return
	}
	err = png.Encode(f, img)
	_ = f.Close()
	if err != nil {
		u.setStatus(err.Error())
		return
	}
	u.setStatus("Saved " + path)
}

func (u *ui) toggleRecord() {
	u.mu.Lock()
	s := u.session
	u.mu.Unlock()
	if s == nil {
		u.setStatus("Start the mirror before recording.")
		return
	}
	if s.Recording() {
		s.StopRecord()
		u.setStatus("Recording saved in Videos\\Luma")
		return
	}
	path := filepath.Join(videosDir(), "luma-"+stamp()+".mp4")
	if err := s.Record(path); err != nil {
		u.setStatus(err.Error())
		return
	}
	u.setStatus("Recording " + path)
}

func (u *ui) installAPK() {
	path, err := pickFile("Android package (*.apk)|*.apk")
	if err != nil || path == "" {
		return
	}
	u.mu.Lock()
	c := u.client
	serial := u.selected
	u.mu.Unlock()
	if c == nil || serial == "" {
		u.setStatus("Select a phone first.")
		return
	}
	u.setStatus("Installing…")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := c.Install(ctx, serial, path); err != nil {
		u.setStatus(err.Error())
		return
	}
	u.setStatus("Installed.")
}

func (u *ui) sendFile() {
	path, err := pickFile("All files|*.*")
	if err != nil || path == "" {
		return
	}
	u.mu.Lock()
	c := u.client
	serial := u.selected
	u.mu.Unlock()
	if c == nil || serial == "" {
		u.setStatus("Select a phone first.")
		return
	}
	u.setStatus("Sending…")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	remote := "/sdcard/Download/" + filepath.Base(path)
	if err := c.Push(ctx, serial, path, remote); err != nil {
		u.setStatus(err.Error())
		return
	}
	u.setStatus("Sent to " + remote)
}

func (u *ui) connectWireless() {
	u.mu.Lock()
	c := u.client
	serial := u.selected
	ip := u.ip.Text()
	u.mu.Unlock()
	if c == nil {
		u.setStatus("Tools are not ready yet.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if ip == "" && serial != "" {
		found, err := c.WLANIP(ctx, serial)
		if err != nil {
			u.setStatus("Type the phone's Wi-Fi address, or plug in once so Luma can read it.")
			return
		}
		ip = found
	}
	if ip == "" {
		u.setStatus("Type the phone's Wi-Fi address.")
		return
	}
	if serial != "" {
		u.setStatus("Opening wireless on the phone…")
		_ = c.TCPIP(ctx, serial)
	}
	addr := ip
	if !containsPort(addr) {
		addr += ":5555"
	}
	if err := c.Connect(ctx, addr); err != nil {
		u.setStatus(err.Error())
		return
	}
	u.mu.Lock()
	u.selected = addr
	u.mu.Unlock()
	u.setStatus("Wireless connected. You can unplug the cable.")
	u.refreshDevices()
}

func containsPort(s string) bool {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return true
		}
		if s[i] == '.' {
			return false
		}
	}
	return false
}

type adbLabel struct {
	serial string
	label  string
}

func (u *ui) labels() []adbLabel {
	out := make([]adbLabel, 0, len(u.devices))
	for _, d := range u.devices {
		out = append(out, adbLabel{serial: d.Serial, label: d.Label()})
	}
	return out
}

func (u *ui) deviceSerials() []string {
	out := make([]string, len(u.devices))
	for i, d := range u.devices {
		out[i] = d.Serial
	}
	return out
}

func (u *ui) row(serial string) *widget.Clickable {
	if u.rows[serial] == nil {
		u.rows[serial] = new(widget.Clickable)
	}
	return u.rows[serial]
}

func (u *ui) btn(gtx layout.Context, c *widget.Clickable, label string, bg, fg color.NRGBA) layout.Dimensions {
	b := material.Button(u.th, c, label)
	b.Background = bg
	b.Color = fg
	b.TextSize = unit.Sp(13)
	b.Inset = layout.Inset{Top: 8, Bottom: 8, Left: 10, Right: 10}
	b.CornerRadius = 8
	return b.Layout(gtx)
}

func (u *ui) qbtn(i, cur int) layout.FlexChild {
	return layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
		bg, fg := colRaise, colText
		if i == cur {
			bg, fg = colAmber, colInk
		}
		return layout.Inset{Right: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return u.btn(gtx, &u.quals[i], mirror.Qualities[i].Name, bg, fg)
		})
	})
}

func row3(gtx layout.Context, u *ui, a *widget.Clickable, al string, b *widget.Clickable, bl string, c *widget.Clickable, cl string) layout.Dimensions {
	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Right: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return u.btn(gtx, a, al, colRaise, colText)
			})
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Right: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return u.btn(gtx, b, bl, colRaise, colText)
			})
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return u.btn(gtx, c, cl, colRaise, colText)
		}),
	)
}

func row2(gtx layout.Context, u *ui, a *widget.Clickable, al string, b *widget.Clickable, bl string) layout.Dimensions {
	return row2c(gtx, u, a, al, colRaise, b, bl, colRaise)
}

func row2c(gtx layout.Context, u *ui, a *widget.Clickable, al string, abg color.NRGBA, b *widget.Clickable, bl string, bbg color.NRGBA) layout.Dimensions {
	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Right: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return u.btn(gtx, a, al, abg, colText)
			})
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			fg := colText
			if bbg == colRec {
				fg = color.NRGBA{R: 255, G: 244, B: 240, A: 255}
			}
			return u.btn(gtx, b, bl, bbg, fg)
		}),
	)
}

func editorBox(gtx layout.Context, th *material.Theme, ed *widget.Editor, hint string) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			paint.FillShape(gtx.Ops, colRaise, clip.RRect{
				Rect: image.Rectangle{Max: gtx.Constraints.Min},
				NE:   8, NW: 8, SE: 8, SW: 8,
			}.Op(gtx.Ops))
			return layout.Dimensions{Size: gtx.Constraints.Min}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				e := material.Editor(th, ed, hint)
				e.Color = colText
				e.HintColor = colMute
				e.TextSize = unit.Sp(14)
				e.Editor.Submit = true
				return e.Layout(gtx)
			})
		}),
	)
}

func rigid(dp unit.Dp) layout.FlexChild {
	return layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		h := gtx.Dp(dp)
		return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, h)}
	})
}
