package mirror

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/elsBlue/luma/internal/adb"
	"github.com/elsBlue/luma/internal/proto"
)

type Quality struct {
	Name    string
	MaxSize int
	Bitrate int
	FPS     int
}

var Qualities = []Quality{
	{Name: "Low", MaxSize: 720, Bitrate: 2_000_000, FPS: 30},
	{Name: "Fair", MaxSize: 1024, Bitrate: 4_000_000, FPS: 30},
	{Name: "High", MaxSize: 1600, Bitrate: 8_000_000, FPS: 60},
	{Name: "Max", MaxSize: 0, Bitrate: 16_000_000, FPS: 60},
}

type Frame struct {
	Image  image.Image
	VideoW int
	VideoH int
	Smooth bool
}

type Session struct {
	adb    *adb.Client
	serial string
	ff     string
	server string
	q      Quality

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	frame   Frame
	smooth  bool
	videoW  int
	videoH  int
	ctrl    net.Conn
	clipSeq uint64
	csd     []byte
	rec     *exec.Cmd
	recIn   io.WriteCloser

	shell *exec.Cmd
	ln    net.Listener
	conns []net.Conn
	ffCmd *exec.Cmd

	linkForward bool
	linkSpec    string
	stopOnce    sync.Once

	jobs    chan func()
	on      func()
	Warning string
}

func Start(parent context.Context, client *adb.Client, serial, ffmpeg, server string, q Quality, onFrame func()) (*Session, error) {
	ctx, cancel := context.WithCancel(parent)
	s := &Session{
		adb:    client,
		serial: serial,
		ff:     ffmpeg,
		server: server,
		q:      q,
		ctx:    ctx,
		cancel: cancel,
		jobs:   make(chan func(), 32),
		on:     onFrame,
	}
	go s.worker()
	err := s.startSmooth()
	if err != nil {
		s.tearStream()
		s.Warning = "Smooth video unavailable (" + trimErr(err) + "). Using a slower compatible capture."
		if err2 := s.startCompat(); err2 != nil {
			s.Stop()
			return nil, fmt.Errorf("%v", err2)
		}
		return s, nil
	}
	s.mu.Lock()
	s.smooth = true
	s.mu.Unlock()
	return s, nil
}

func (s *Session) Smooth() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.smooth
}

func (s *Session) Frame() Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frame
}

func (s *Session) Stop() {
	s.stopOnce.Do(func() {
		s.cancel()
		s.stopRecord()
		s.tearStream()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = s.adb.Shell(ctx, s.serial, "pkill -f luma-server.jar")
		cancel()
		close(s.jobs)
	})
}

func (s *Session) worker() {
	for fn := range s.jobs {
		fn()
	}
}

func (s *Session) enqueue(fn func()) {
	defer func() { _ = recover() }()
	select {
	case <-s.ctx.Done():
	case s.jobs <- fn:
	default:
		go fn()
	}
}

func (s *Session) Touch(action byte, x, y, w, h int) {
	if w <= 0 || h <= 0 {
		return
	}
	s.mu.Lock()
	smooth := s.smooth
	s.mu.Unlock()
	if smooth {
		var pressure float32 = 1
		var actionButton, buttons uint32
		switch action {
		case proto.ActionDown:
			actionButton, buttons = proto.ButtonPrimary, proto.ButtonPrimary
		case proto.ActionMove:
			buttons = proto.ButtonPrimary
		default:
			pressure = 0
			actionButton = proto.ButtonPrimary
		}
		msg := proto.TouchMessage(action, x, y, w, h, pressure, actionButton, buttons)
		s.writeCtrl(msg)
		return
	}
	if action != proto.ActionUp && action != proto.ActionDown {
		return
	}
	if action == proto.ActionUp {
		s.enqueue(func() {
			_, _ = s.adb.Shell(s.ctx, s.serial, fmt.Sprintf("input tap %d %d", x, y))
		})
	}
}

func (s *Session) Swipe(x0, y0, x1, y1, w, h int) {
	s.mu.Lock()
	smooth := s.smooth
	s.mu.Unlock()
	if smooth || w <= 0 {
		return
	}
	s.enqueue(func() {
		_, _ = s.adb.Shell(s.ctx, s.serial, fmt.Sprintf("input swipe %d %d %d %d 120", x0, y0, x1, y1))
	})
}

func (s *Session) Scroll(x, y, w, h int, dy float32) {
	if w <= 0 || h <= 0 || dy == 0 {
		return
	}
	s.mu.Lock()
	smooth := s.smooth
	s.mu.Unlock()
	if smooth {
		s.writeCtrl(proto.ScrollMessage(x, y, w, h, dy))
		return
	}
	s.enqueue(func() {
		y2 := y + int(dy*80)
		_, _ = s.adb.Shell(s.ctx, s.serial, fmt.Sprintf("input swipe %d %d %d %d 80", x, y, x, y2))
	})
}

func (s *Session) Key(code uint32) {
	s.mu.Lock()
	smooth := s.smooth
	s.mu.Unlock()
	if smooth {
		s.writeCtrl(proto.KeyMessage(proto.ActionDown, code, 0, 0))
		s.writeCtrl(proto.KeyMessage(proto.ActionUp, code, 0, 0))
		return
	}
	s.enqueue(func() {
		_, _ = s.adb.Shell(s.ctx, s.serial, fmt.Sprintf("input keyevent %d", code))
	})
}

func (s *Session) Text(text string) {
	if text == "" {
		return
	}
	s.mu.Lock()
	smooth := s.smooth
	s.mu.Unlock()
	if smooth {
		s.writeCtrl(proto.TextMessage(text))
		return
	}
	safe := strings.ReplaceAll(text, " ", "%s")
	safe = strings.ReplaceAll(safe, "'", "")
	s.enqueue(func() {
		_, _ = s.adb.Shell(s.ctx, s.serial, "input text '"+safe+"'")
	})
}

func (s *Session) Back() { s.Key(proto.KeyBack) }

func (s *Session) Rotate() {
	s.mu.Lock()
	smooth := s.smooth
	s.mu.Unlock()
	if !smooth {
		return
	}
	s.writeCtrl(proto.Simple(proto.MsgRotate))
}

func (s *Session) Paste(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	s.mu.Lock()
	smooth := s.smooth
	s.clipSeq++
	seq := s.clipSeq
	s.mu.Unlock()
	if smooth {
		s.writeCtrl(proto.ClipboardMessage(seq, text, true))
		return
	}
	s.Text(text)
}

func (s *Session) writeCtrl(msg []byte) {
	s.mu.Lock()
	c := s.ctrl
	s.mu.Unlock()
	if c == nil {
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = c.Write(msg)
}

func (s *Session) rememberLink(forward bool, spec string) {
	s.mu.Lock()
	s.linkForward = forward
	s.linkSpec = spec
	s.mu.Unlock()
}

func (s *Session) startSmooth() error {
	ctx, cancel := context.WithTimeout(s.ctx, 45*time.Second)
	defer cancel()
	if err := s.adb.Push(ctx, s.serial, s.server, "/data/local/tmp/luma-server.jar"); err != nil {
		return fmt.Errorf("could not copy the phone service: %w", err)
	}
	var first error
	if err := s.stream(false); err != nil {
		first = err
		s.tearStream()
		if err2 := s.stream(true); err2 != nil {
			s.tearStream()
			return fmt.Errorf("%v", first)
		}
	}
	return nil
}

func (s *Session) stream(forward bool) error {
	scid, socket := newSCID()
	port, err := freePort()
	if err != nil {
		return err
	}
	ctx := s.ctx
	if forward {
		spec := fmt.Sprintf("tcp:%d", port)
		if err := s.adb.Forward(ctx, s.serial, spec, "localabstract:"+socket); err != nil {
			return err
		}
		s.rememberLink(true, spec)
	} else {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.ln = ln
		s.mu.Unlock()
		spec := "localabstract:" + socket
		if err := s.adb.Reverse(ctx, s.serial, spec, fmt.Sprintf("tcp:%d", port)); err != nil {
			return err
		}
		s.rememberLink(false, spec)
	}
	cmdLine := fmt.Sprintf(
		"CLASSPATH=/data/local/tmp/luma-server.jar app_process / com.genymobile.scrcpy.Server %s scid=%s log_level=info tunnel_forward=%t audio=false control=true video=true video_codec=h264 max_size=%d max_fps=%d video_bit_rate=%d stay_awake=true power_on=true",
		proto.ServerVersion, scid, forward, s.q.MaxSize, s.q.FPS, s.q.Bitrate,
	)
	shell, stdout, err := s.adb.StartShell(ctx, s.serial, cmdLine)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.shell = shell
	s.mu.Unlock()
	go func() {
		_, _ = io.Copy(io.Discard, stdout)
	}()

	var video, ctrl net.Conn
	if forward {
		video, err = dialRetry(ctx, port, 8*time.Second)
		if err != nil {
			return fmt.Errorf("phone did not open video: %w", err)
		}
		ctrl, err = dialRetry(ctx, port, 4*time.Second)
		if err != nil {
			_ = video.Close()
			return fmt.Errorf("phone did not open control: %w", err)
		}
	} else {
		if tcp, ok := s.ln.(*net.TCPListener); ok {
			_ = tcp.SetDeadline(time.Now().Add(8 * time.Second))
		}
		video, err = s.ln.Accept()
		if err != nil {
			return fmt.Errorf("phone did not connect video: %w", err)
		}
		ctrl, err = s.ln.Accept()
		if err != nil {
			_ = video.Close()
			return fmt.Errorf("phone did not connect control: %w", err)
		}
	}
	s.mu.Lock()
	s.conns = []net.Conn{video, ctrl}
	s.ctrl = ctrl
	s.mu.Unlock()

	_ = video.SetReadDeadline(time.Now().Add(6 * time.Second))
	if forward {
		if _, err := io.ReadFull(video, make([]byte, 1)); err != nil {
			return fmt.Errorf("no handshake: %w", err)
		}
	}
	name := make([]byte, 64)
	if _, err := io.ReadFull(video, name); err != nil {
		return fmt.Errorf("no device name: %w", err)
	}
	_ = video.SetReadDeadline(time.Time{})
	if err := s.startDecoder(video); err != nil {
		return err
	}
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-time.After(8 * time.Second):
		s.mu.Lock()
		ok := s.frame.Image != nil
		s.mu.Unlock()
		if !ok {
			return fmt.Errorf("no picture arrived")
		}
	case <-s.waitFrame(8 * time.Second):
	}
	_ = name
	return nil
}

func (s *Session) waitFrame(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		dead := time.Now().Add(d)
		for time.Now().Before(dead) {
			s.mu.Lock()
			ok := s.frame.Image != nil
			s.mu.Unlock()
			if ok {
				close(ch)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	return ch
}

func (s *Session) startDecoder(video net.Conn) error {
	ff := exec.CommandContext(s.ctx, s.ff,
		"-hide_banner", "-loglevel", "error",
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-probesize", "32",
		"-analyzeduration", "0",
		"-f", "h264",
		"-i", "pipe:0",
		"-an",
		"-q:v", "4",
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"pipe:1",
	)
	hide(ff)
	stdin, err := ff.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := ff.StdoutPipe()
	if err != nil {
		return err
	}
	var ferr bytes.Buffer
	ff.Stderr = &ferr
	if err := ff.Start(); err != nil {
		return fmt.Errorf("decoder: %w", err)
	}
	s.mu.Lock()
	s.ffCmd = ff
	s.mu.Unlock()
	go s.pumpVideo(video, stdin)
	go s.pumpJPEG(stdout)
	go func() {
		_ = ff.Wait()
	}()
	return nil
}

func (s *Session) pumpVideo(r net.Conn, w io.WriteCloser) {
	defer w.Close()
	br := bufio.NewReaderSize(r, 256*1024)
	codec := make([]byte, 4)
	if _, err := io.ReadFull(br, codec); err != nil {
		return
	}
	id := binary.BigEndian.Uint32(codec)
	if id != proto.CodecH264 && id != proto.CodecH265 {
		return
	}
	hdr := make([]byte, 12)
	for s.ctx.Err() == nil {
		if _, err := io.ReadFull(br, hdr); err != nil {
			return
		}
		h, err := proto.ParseVideoHeader(hdr)
		if err != nil {
			return
		}
		if h.Session {
			if h.Width > 0 && h.Height > 0 {
				s.mu.Lock()
				s.videoW, s.videoH = h.Width, h.Height
				s.mu.Unlock()
			}
			continue
		}
		if h.Size <= 0 || h.Size > 8<<20 {
			return
		}
		payload := make([]byte, h.Size)
		if _, err := io.ReadFull(br, payload); err != nil {
			return
		}
		annex := proto.AnnexB(payload)
		s.mu.Lock()
		if h.Config {
			s.csd = append([]byte(nil), annex...)
		}
		csd := append([]byte(nil), s.csd...)
		rec := s.recIn
		s.mu.Unlock()
		var blob []byte
		if h.Config {
			blob = annex
		} else if h.Key && len(csd) > 0 && !proto.HasSPS(annex) {
			blob = append(append([]byte{}, csd...), annex...)
		} else {
			blob = annex
		}
		_, _ = w.Write(blob)
		if rec != nil && !h.Config {
			_, _ = rec.Write(blob)
		}
	}
}

func (s *Session) pumpJPEG(r io.Reader) {
	br := bufio.NewReaderSize(r, 512*1024)
	for s.ctx.Err() == nil {
		raw, err := readJPEG(br)
		if err != nil {
			return
		}
		img, err := jpeg.Decode(bytes.NewReader(raw))
		if err != nil {
			continue
		}
		s.mu.Lock()
		s.frame = Frame{Image: img, VideoW: s.videoW, VideoH: s.videoH, Smooth: true}
		if s.frame.VideoW == 0 {
			s.frame.VideoW = img.Bounds().Dx()
			s.frame.VideoH = img.Bounds().Dy()
		}
		s.mu.Unlock()
		if s.on != nil {
			s.on()
		}
	}
}

func (s *Session) tearStream() {
	s.mu.Lock()
	s.frame = Frame{}
	conns := s.conns
	s.conns = nil
	ctrl := s.ctrl
	s.ctrl = nil
	ln := s.ln
	s.ln = nil
	ff := s.ffCmd
	s.ffCmd = nil
	shell := s.shell
	s.shell = nil
	forward := s.linkForward
	spec := s.linkSpec
	s.linkSpec = ""
	serial := s.serial
	s.mu.Unlock()
	if ctrl != nil {
		_ = ctrl.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	if ln != nil {
		_ = ln.Close()
	}
	if ff != nil && ff.Process != nil {
		_ = ff.Process.Kill()
	}
	if shell != nil && shell.Process != nil {
		_ = shell.Process.Kill()
	}
	if spec != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if forward {
			s.adb.ForwardRemove(ctx, serial, spec)
		} else {
			s.adb.ReverseRemove(ctx, serial, spec)
		}
		cancel()
	}
}

func (s *Session) startCompat() error {
	go func() {
		for s.ctx.Err() == nil {
			cmd := s.adb.ExecOut(s.ctx, s.serial, "screencap", "-p")
			out, err := cmd.Output()
			if err != nil || s.ctx.Err() != nil {
				time.Sleep(300 * time.Millisecond)
				continue
			}
			img, err := decodePNG(out)
			if err != nil {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			b := img.Bounds()
			s.mu.Lock()
			s.frame = Frame{Image: img, VideoW: b.Dx(), VideoH: b.Dy(), Smooth: false}
			s.videoW, s.videoH = b.Dx(), b.Dy()
			s.mu.Unlock()
			if s.on != nil {
				s.on()
			}
		}
	}()
	dead := time.Now().Add(6 * time.Second)
	for time.Now().Before(dead) {
		s.mu.Lock()
		ok := s.frame.Image != nil
		s.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-time.After(80 * time.Millisecond):
		}
	}
	return fmt.Errorf("could not capture the phone screen. Unlock it, enable USB debugging, and accept the prompt")
}

func (s *Session) Record(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.smooth {
		return fmt.Errorf("recording needs the smooth stream. Reconnect on High or Max")
	}
	if s.rec != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	cmd := exec.Command(s.ff,
		"-y", "-fflags", "+genpts",
		"-f", "h264", "-i", "pipe:0",
		"-c", "copy",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		path,
	)
	hide(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if len(s.csd) > 0 {
		_, _ = in.Write(s.csd)
	}
	s.rec = cmd
	s.recIn = in
	return nil
}

func (s *Session) StopRecord() {
	s.stopRecord()
}

func (s *Session) stopRecord() {
	s.mu.Lock()
	cmd := s.rec
	in := s.recIn
	s.rec = nil
	s.recIn = nil
	s.mu.Unlock()
	if cmd == nil {
		return
	}
	_ = in.Close()
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
}

func (s *Session) Recording() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec != nil
}

func newSCID() (string, string) {
	var b [4]byte
	_, _ = rand.Read(b[:])
	b[0] &= 0x7f
	scid := fmt.Sprintf("%08x", binary.BigEndian.Uint32(b[:]))
	return scid, "scrcpy_" + scid
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

func dialRetry(ctx context.Context, port int, d time.Duration) (net.Conn, error) {
	dead := time.Now().Add(d)
	var last error
	for time.Now().Before(dead) {
		dialer := net.Dialer{Timeout: 400 * time.Millisecond}
		c, err := dialer.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return c, nil
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(120 * time.Millisecond):
		}
	}
	if last == nil {
		last = fmt.Errorf("timeout")
	}
	return nil, last
}

func readJPEG(r *bufio.Reader) ([]byte, error) {
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b != 0xff {
			continue
		}
		n, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if n != 0xd8 {
			if n == 0xff {
				_ = r.UnreadByte()
			}
			continue
		}
		buf := []byte{0xff, 0xd8}
		prev := byte(0)
		for {
			c, err := r.ReadByte()
			if err != nil {
				return nil, err
			}
			buf = append(buf, c)
			if prev == 0xff && c == 0xd9 {
				return buf, nil
			}
			prev = c
			if len(buf) > 8<<20 {
				return nil, fmt.Errorf("frame too large")
			}
		}
	}
}

func decodePNG(b []byte) (image.Image, error) {
	if i := bytes.Index(b, []byte("\x89PNG\r\n\x1a\n")); i > 0 {
		b = b[i:]
	}
	return png.Decode(bytes.NewReader(b))
}

func trimErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 180 {
		return s[:180]
	}
	return s
}
