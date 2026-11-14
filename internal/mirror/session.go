package mirror

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
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
	{Name: "Fast", MaxSize: 960, Bitrate: 4_000_000, FPS: 60},
	{Name: "720p", MaxSize: 1280, Bitrate: 6_000_000, FPS: 60},
	{Name: "1080p", MaxSize: 1920, Bitrate: 8_000_000, FPS: 60},
	{Name: "YouTube", MaxSize: 1920, Bitrate: 12_000_000, FPS: 60},
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

	moveMu      sync.Mutex
	movePending []byte
	moveTimer   *time.Timer

	ptsBase  uint64
	wallBase time.Time
	lag      time.Duration

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
		if action == proto.ActionMove {
			s.queueMove(msg)
			return
		}
		s.flushMove()
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

func (s *Session) queueMove(msg []byte) {
	s.moveMu.Lock()
	s.movePending = msg
	if s.moveTimer == nil {
		s.moveTimer = time.AfterFunc(time.Millisecond, func() {
			s.moveMu.Lock()
			s.moveTimer = nil
			msg := s.movePending
			s.movePending = nil
			s.moveMu.Unlock()
			if msg != nil {
				s.writeCtrl(msg)
			}
		})
	}
	s.moveMu.Unlock()
}

func (s *Session) flushMove() {
	s.moveMu.Lock()
	if s.moveTimer != nil {
		s.moveTimer.Stop()
		s.moveTimer = nil
	}
	msg := s.movePending
	s.movePending = nil
	s.moveMu.Unlock()
	if msg != nil {
		s.writeCtrl(msg)
	}
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
	for _, opts := range codecOptionTiers(s.q.FPS) {
		err := s.tryLink(opts)
		if err == nil {
			if opts == "" {
				s.Warning = "This phone rejected the low-delay encoder flags. The picture still updates, but it may lag more."
			}
			return nil
		}
		first = err
		s.tearStream()
	}
	if first == nil {
		first = fmt.Errorf("could not open the video stream")
	}
	return fmt.Errorf("%v", first)
}

func codecOptionTiers(fps int) []string {
	if fps <= 0 {
		fps = 60
	}
	// Repeat interval is in microseconds. One frame period keeps a still
	// screen from freezing on the first picture.
	repeatUS := 1000000 / fps
	// Constrained Baseline, no B-frames, keyframe about once a second.
	// All-intra (interval 0) makes every frame huge and slower, not faster.
	// low-latency=1 is the Android 11 encoder flag; latency=0 is the frame count.
	full := fmt.Sprintf(
		"profile:int=65536,max-bframes:int=0,i-frame-interval:float=1,low-latency:int=1,latency:int=0,priority:int=0,operating-rate:int=%d,repeat-previous-frame-after:long=%d,vendor.qti-ext-enc-low-latency.enable:int=1",
		fps, repeatUS,
	)
	mid := fmt.Sprintf(
		"max-bframes:int=0,i-frame-interval:float=1,latency:int=0,priority:int=0,operating-rate:int=%d,repeat-previous-frame-after:long=%d",
		fps, repeatUS,
	)
	basic := "max-bframes:int=0,latency:int=0,priority:int=0,low-latency:int=1"
	return []string{full, mid, basic, ""}
}

func (s *Session) tryLink(opts string) error {
	if err := s.stream(false, opts); err != nil {
		s.tearStream()
		return s.stream(true, opts)
	}
	return nil
}

func (s *Session) stream(forward bool, opts string) error {
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
	codec := ""
	if opts != "" {
		codec = " video_codec_options=" + opts
	}
	cmdLine := fmt.Sprintf(
		"CLASSPATH=/data/local/tmp/luma-server.jar app_process / com.genymobile.scrcpy.Server %s scid=%s log_level=info tunnel_forward=%t audio=false control=true video=true video_codec=h264 max_size=%d max_fps=%d video_bit_rate=%d stay_awake=true power_on=true send_frame_meta=true send_codec_meta=true send_device_meta=true%s",
		proto.ServerVersion, scid, forward, cappedSize(s.q.MaxSize), s.q.FPS, s.q.Bitrate, codec,
	)
	shell, stdout, err := s.adb.StartShell(ctx, s.serial, cmdLine)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.shell = shell
	s.mu.Unlock()
	shellDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, stdout)
		close(shellDone)
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
	tune(video)
	tune(ctrl)

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
	case <-shellDone:
		return fmt.Errorf("phone encoder stopped")
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
	go s.pumpVideo(video)
	return nil
}

func (s *Session) pumpVideo(r net.Conn) {
	sr := &streamReader{c: r}
	codec, err := sr.takeBlocking(4)
	if err != nil {
		return
	}
	id := binary.BigEndian.Uint32(codec)
	if id != proto.CodecH264 && id != proto.CodecH265 {
		return
	}
	var dec io.WriteCloser
	defer func() {
		if dec != nil {
			_ = dec.Close()
		}
	}()
	var priming [][]byte
	for s.ctx.Err() == nil {
		batch, err := s.readCatchUp(sr)
		if err != nil || len(batch) == 0 {
			return
		}
		var recBlobs [][]byte
		var view []viewPkt
		for _, item := range batch {
			if item.header.Session {
				if item.header.Width > 0 && item.header.Height > 0 {
					s.mu.Lock()
					s.videoW, s.videoH = item.header.Width, item.header.Height
					s.mu.Unlock()
					if dec == nil {
						in, err := s.startRawDecoder(item.header.Width, item.header.Height)
						if err != nil {
							return
						}
						dec = in
						for _, blob := range priming {
							writeLive(dec, blob)
						}
						priming = nil
					}
				}
				continue
			}
			blob := s.annexOf(item)
			if blob == nil {
				return
			}
			if !item.header.Config {
				recBlobs = append(recBlobs, blob)
				if item.header.PTS > 0 {
					s.notePTS(item.header.PTS)
				}
			}
			view = append(view, viewPkt{config: item.header.Config, key: item.header.Key && !item.header.Config, blob: blob})
		}
		show := displayBlobs(view)
		if dec == nil {
			priming = append(priming, show...)
			if len(priming) > 48 {
				return
			}
			continue
		}
		for _, blob := range show {
			writeLive(dec, blob)
		}
		s.mu.Lock()
		rec := s.recIn
		s.mu.Unlock()
		if rec != nil {
			for _, blob := range recBlobs {
				_, _ = rec.Write(blob)
			}
		}
	}
}

type wirePkt struct {
	header  proto.VideoHeader
	payload []byte
}

func (s *Session) readCatchUp(sr *streamReader) ([]wirePkt, error) {
	first, err := sr.packet()
	if err != nil {
		return nil, err
	}
	batch := []wirePkt{first}
	for sr.moreSoon() {
		next, err := sr.packet()
		if err != nil {
			if timedOut(err) {
				break
			}
			return batch, err
		}
		batch = append(batch, next)
		if len(batch) >= 90 {
			break
		}
	}
	return batch, nil
}

func (s *Session) annexOf(item wirePkt) []byte {
	h := item.header
	if h.Session {
		return nil
	}
	if h.Size <= 0 || h.Size > 8<<20 || len(item.payload) != h.Size {
		return nil
	}
	annex := proto.AnnexB(item.payload)
	s.mu.Lock()
	if h.Config {
		s.csd = append([]byte(nil), annex...)
	}
	csd := append([]byte(nil), s.csd...)
	s.mu.Unlock()
	if h.Config {
		return annex
	}
	if h.Key && len(csd) > 0 && !proto.HasSPS(annex) {
		return append(append([]byte{}, csd...), annex...)
	}
	return annex
}

func writeLive(w io.Writer, blob []byte) {
	if len(blob) == 0 {
		return
	}
	_, _ = w.Write(blob)
	// An access-unit delimiter lets the decoder emit this frame now,
	// instead of waiting until the next one arrives.
	_, _ = w.Write([]byte{0, 0, 0, 1, 0x09, 0xf0})
}

func (s *Session) notePTS(pts uint64) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptsBase == 0 {
		s.ptsBase = pts
		s.wallBase = now
		return
	}
	if pts < s.ptsBase {
		return
	}
	lag := now.Sub(s.wallBase) - time.Duration(pts-s.ptsBase)*time.Microsecond
	if lag < 0 {
		lag = 0
	}
	s.lag = lag
}

func (s *Session) Lag() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lag
}

type viewPkt struct {
	config bool
	key    bool
	blob   []byte
}

func displayBlobs(batch []viewPkt) [][]byte {
	lastKey := -1
	var cfg []byte
	for i, p := range batch {
		if p.config && len(p.blob) > 0 {
			cfg = p.blob
		}
		if p.key {
			lastKey = i
		}
	}
	if lastKey < 0 {
		out := make([][]byte, 0, len(batch))
		for _, p := range batch {
			if len(p.blob) > 0 && !p.config {
				out = append(out, p.blob)
			}
		}
		if cfg != nil && len(out) > 0 {
			return append([][]byte{cfg}, out...)
		}
		if cfg != nil {
			return [][]byte{cfg}
		}
		return out
	}
	out := make([][]byte, 0, len(batch)-lastKey+1)
	if cfg != nil {
		out = append(out, cfg)
	}
	for _, p := range batch[lastKey:] {
		if p.config || len(p.blob) == 0 {
			continue
		}
		out = append(out, p.blob)
	}
	return out
}

type streamReader struct {
	c   net.Conn
	buf []byte
}

func (s *streamReader) takeBlocking(n int) ([]byte, error) {
	if err := s.readAtLeast(n); err != nil {
		return nil, err
	}
	return s.take(n), nil
}

func (s *streamReader) packet() (wirePkt, error) {
	hdr, err := s.takeBlocking(12)
	if err != nil {
		return wirePkt{}, err
	}
	h, err := proto.ParseVideoHeader(hdr)
	if err != nil {
		return wirePkt{}, err
	}
	if h.Session {
		return wirePkt{header: h}, nil
	}
	if h.Size <= 0 || h.Size > 8<<20 {
		return wirePkt{}, fmt.Errorf("bad packet size")
	}
	payload, err := s.takeBlocking(h.Size)
	if err != nil {
		return wirePkt{}, err
	}
	return wirePkt{header: h, payload: payload}, nil
}

func (s *streamReader) moreSoon() bool {
	if len(s.buf) >= 12 {
		return true
	}
	_ = s.c.SetReadDeadline(time.Now().Add(300 * time.Microsecond))
	tmp := make([]byte, 64*1024)
	n, err := s.c.Read(tmp)
	_ = s.c.SetReadDeadline(time.Time{})
	if n > 0 {
		s.buf = append(s.buf, tmp[:n]...)
	}
	if err != nil && !timedOut(err) {
		return false
	}
	return len(s.buf) >= 12
}

func (s *streamReader) readAtLeast(n int) error {
	for len(s.buf) < n {
		_ = s.c.SetReadDeadline(time.Time{})
		tmp := make([]byte, 32*1024)
		k, err := s.c.Read(tmp)
		if k > 0 {
			s.buf = append(s.buf, tmp[:k]...)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *streamReader) take(n int) []byte {
	out := make([]byte, n)
	copy(out, s.buf[:n])
	rest := len(s.buf) - n
	copy(s.buf[:rest], s.buf[n:])
	s.buf = s.buf[:rest]
	return out
}

func timedOut(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (s *Session) startRawDecoder(w, h int) (io.WriteCloser, error) {
	in, err := s.spawnDecoder(w, h, true)
	if err != nil {
		return s.spawnDecoder(w, h, false)
	}
	return in, nil
}

func (s *Session) spawnDecoder(w, h int, bsf bool) (io.WriteCloser, error) {
	if w < 2 || h < 2 || w > 1920 || h > 1920 {
		return nil, fmt.Errorf("frame %dx%d is outside the 1080p cap", w, h)
	}
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-fflags", "nobuffer",
		"-flags", "+low_delay",
		// The default probe holds the first picture for about a second.
		// A short cap still parses the stream; 0 and a tiny probesize drop every frame.
		"-probesize", "5000000",
		"-analyzeduration", "100000",
		"-fpsprobesize", "1",
		// Slice threads speed 1080p up. low_delay above stops frame threading,
		// which would add one frame of delay per thread.
		"-threads", "4",
		"-thread_type", "slice",
		"-f", "h264",
	}
	if bsf {
		args = append(args, "-bsf:v", "h264_metadata=num_reorder_frames=0:max_dec_frame_buffering=1")
	}
	args = append(args,
		"-i", "pipe:0",
		"-an",
		"-sws_flags", "fast_bilinear",
		"-vsync", "0",
		"-muxdelay", "0",
		"-muxpreload", "0",
		"-pix_fmt", "rgba",
		"-f", "rawvideo",
		"pipe:1",
	)
	ff := exec.CommandContext(s.ctx, s.ff, args...)
	hide(ff)
	stdin, err := ff.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := ff.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var ferr bytes.Buffer
	ff.Stderr = &ferr
	if err := ff.Start(); err != nil {
		return nil, fmt.Errorf("decoder: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = ff.Wait()
		close(exited)
	}()
	select {
	case <-exited:
		msg := strings.TrimSpace(ferr.String())
		if msg == "" {
			msg = "decoder stopped"
		}
		return nil, fmt.Errorf("%s", trimErr(errors.New(msg)))
	case <-time.After(200 * time.Millisecond):
	}
	s.mu.Lock()
	s.ffCmd = ff
	s.mu.Unlock()
	go s.pumpRaw(stdout, w, h)
	return stdin, nil
}

func (s *Session) pumpRaw(r io.Reader, w, h int) {
	stride := w * h * 4
	br := bufio.NewReaderSize(r, stride)
	file, _ := r.(*os.File)
	imgs := [3]*image.RGBA{
		image.NewRGBA(image.Rect(0, 0, w, h)),
		image.NewRGBA(image.Rect(0, 0, w, h)),
		image.NewRGBA(image.Rect(0, 0, w, h)),
	}
	slot := 0
	skip := make([]byte, stride)
	for s.ctx.Err() == nil {
		for {
			extra := br.Buffered() + pipePending(file)
			if extra < stride*2 {
				break
			}
			if _, err := io.ReadFull(br, skip); err != nil {
				return
			}
		}
		if _, err := io.ReadFull(br, imgs[slot].Pix); err != nil {
			return
		}
		img := imgs[slot]
		slot = (slot + 1) % len(imgs)
		s.mu.Lock()
		vw, vh := s.videoW, s.videoH
		if vw == 0 {
			vw, vh = w, h
		}
		s.frame = Frame{Image: img, VideoW: vw, VideoH: vh, Smooth: true}
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
		return fmt.Errorf("recording needs the smooth stream. Reconnect on 1080p or YouTube")
	}
	if s.rec != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	cmd := exec.Command(s.ff,
		"-y",
		"-use_wallclock_as_timestamps", "1",
		"-fflags", "+genpts",
		"-f", "h264",
		"-i", "pipe:0",
		"-c", "copy",
		"-movflags", "+faststart",
		"-metadata", "comment=Luma 1080p",
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

func cappedSize(n int) int {
	if n <= 0 || n > 1920 {
		return 1920
	}
	return n
}

func tune(c net.Conn) {
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tcp.SetNoDelay(true)
	_ = tcp.SetKeepAlive(false)
	_ = tcp.SetReadBuffer(256 * 1024)
	_ = tcp.SetWriteBuffer(32 * 1024)
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
