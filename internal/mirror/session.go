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

	tier      string
	encoder   string
	usbWait   time.Duration
	decQ      []time.Time
	decLast   time.Duration
	frameAt   time.Time
	fps       int
	fpsN      int
	fpsT      time.Time
	shot      time.Duration
	usbMax    time.Duration
	sentN     int
	logf      *os.File
	logPath   string
	logAt     time.Time
	logClosed bool

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
		s.mu.Lock()
		s.tier = "capture"
		s.mu.Unlock()
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
		s.mu.Lock()
		s.logClosed = true
		if s.logf != nil {
			_ = s.logf.Close()
			s.logf = nil
		}
		s.mu.Unlock()
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

func (s *Session) pickEncoder(ctx context.Context) string {
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := fmt.Sprintf(
		"CLASSPATH=/data/local/tmp/luma-server.jar app_process / com.genymobile.scrcpy.Server %s log_level=info list_encoders=true",
		proto.ServerVersion,
	)
	out, _ := s.adb.Shell(cctx, s.serial, cmd)
	name := chooseH264Encoder(out)
	s.mu.Lock()
	s.encoder = shortEncoder(name)
	s.mu.Unlock()
	return name
}

func chooseH264Encoder(log string) string {
	var cands []string
	for _, line := range strings.Split(log, "\n") {
		if fieldAfter(line, "video-codec=") != "h264" {
			continue
		}
		name := fieldAfter(line, "video-encoder=")
		if name == "" || !safeEncoderName(name) || strings.Contains(strings.ToLower(line), "alias for") {
			continue
		}
		if softwareEncoder(name) || strings.Contains(line, "(sw)") {
			continue
		}
		if strings.Contains(line, "(hw)") || strings.Contains(line, "(hybrid)") || hardwareEncoder(name) {
			cands = append(cands, name)
		}
	}
	if len(cands) == 0 {
		return ""
	}
	best := cands[0]
	for _, name := range cands[1:] {
		if encoderScore(name) > encoderScore(best) {
			best = name
		}
	}
	return best
}

func fieldAfter(line, key string) string {
	i := strings.Index(line, key)
	if i < 0 {
		return ""
	}
	rest := line[i+len(key):]
	end := strings.IndexAny(rest, " \t\r")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func safeEncoderName(name string) bool {
	if name == "" || len(name) > 80 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func softwareEncoder(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "omx.google") || strings.Contains(n, "c2.android") || strings.Contains(n, "c2.google")
}

func hardwareEncoder(name string) bool {
	n := strings.ToLower(name)
	for _, k := range []string{"qti", "qcom", "exynos", "mtk", "mediatek", ".sec", "samsung", "unisoc", "sprd", "hisi", "amlogic", "nvidia", "v4l2"} {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

func encoderScore(name string) int {
	n := strings.ToLower(name)
	score := 10
	switch {
	case strings.Contains(n, "qti") || strings.Contains(n, "qcom"):
		score = 80
	case strings.Contains(n, "exynos") || strings.Contains(n, ".sec"):
		score = 70
	case strings.Contains(n, "mtk") || strings.Contains(n, "mediatek"):
		score = 70
	case strings.Contains(n, "unisoc") || strings.Contains(n, "sprd"):
		score = 60
	}
	if strings.HasPrefix(n, "c2.") {
		score += 5
	}
	if strings.Contains(n, "lowlatency") || strings.Contains(n, "low_latency") {
		score += 15
	}
	if strings.Contains(n, "secure") {
		score -= 20
	}
	return score
}

func shortEncoder(name string) string {
	n := strings.ToLower(name)
	switch {
	case name == "":
		return ""
	case strings.Contains(n, "qti") || strings.Contains(n, "qcom"):
		return "qcom"
	case strings.Contains(n, "mtk") || strings.Contains(n, "mediatek"):
		return "mtk"
	case strings.Contains(n, "exynos"):
		return "exynos"
	case strings.Contains(n, ".sec"):
		return "sec"
	}
	if len(name) > 18 {
		return name[:18]
	}
	return name
}

func (s *Session) startSmooth() error {
	ctx, cancel := context.WithTimeout(s.ctx, 45*time.Second)
	defer cancel()
	if err := s.adb.Push(ctx, s.serial, s.server, "/data/local/tmp/luma-server.jar"); err != nil {
		return fmt.Errorf("could not copy the phone service: %w", err)
	}
	enc := s.pickEncoder(ctx)
	err := s.openTiers(enc)
	if err != nil && enc != "" && ctx.Err() == nil {
		s.tearStream()
		err = s.openTiers("")
	}
	return err
}

func (s *Session) openTiers(encoder string) error {
	var first error
	tiers := codecOptionTiers(s.q.FPS)
	names := []string{"hw", "std", "basic", "default"}
	for i, opts := range tiers {
		err := s.tryLink(opts, encoder)
		if err == nil {
			name := "default"
			if i < len(names) {
				name = names[i]
			}
			s.mu.Lock()
			s.tier = name
			if encoder == "" {
				s.encoder = ""
			}
			s.mu.Unlock()
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
	// 100ms, same as a normal live encoder. Repeating every frame period
	// injects duplicate pictures and makes motion look stepped.
	repeatUS := 100000
	// Keyframe every 10s. Every 1s the IDR is large enough to hitch the picture.
	// No forced profile: Constrained Baseline can push the phone onto a slow
	// software encoder, which sits around half a second and stutters.
	// latency=1 is the smallest value Android will honor: one frame in,
	// one frame out. 0 is ignored or treated as a deep buffer.
	hw := fmt.Sprintf(
		"max-bframes:int=0,i-frame-interval:float=10,low-latency:int=1,latency:int=1,priority:int=0,operating-rate:int=%d,repeat-previous-frame-after:long=%d,vendor.qti-ext-enc-low-latency.enable:int=1",
		fps, repeatUS,
	)
	std := fmt.Sprintf(
		"max-bframes:int=0,i-frame-interval:float=10,low-latency:int=1,latency:int=1,priority:int=0,operating-rate:int=%d,repeat-previous-frame-after:long=%d",
		fps, repeatUS,
	)
	basic := "max-bframes:int=0,latency:int=1,priority:int=0,low-latency:int=1"
	return []string{hw, std, basic, ""}
}

func (s *Session) tryLink(opts, encoder string) error {
	if err := s.stream(false, opts, encoder); err != nil {
		s.tearStream()
		return s.stream(true, opts, encoder)
	}
	return nil
}

func (s *Session) stream(forward bool, opts, encoder string) error {
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
	extra := ""
	if opts != "" {
		extra += " video_codec_options=" + opts
	}
	if encoder != "" {
		extra += " video_encoder=" + encoder
	}
	cmdLine := fmt.Sprintf(
		"CLASSPATH=/data/local/tmp/luma-server.jar app_process / com.genymobile.scrcpy.Server %s scid=%s log_level=info tunnel_forward=%t audio=false control=true video=true video_codec=h264 max_size=%d max_fps=%d video_bit_rate=%d stay_awake=true power_on=true send_frame_meta=true send_codec_meta=true send_device_meta=true%s",
		proto.ServerVersion, scid, forward, cappedSize(s.q.MaxSize), s.q.FPS, s.q.Bitrate, extra,
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
	var primingPics int
	for s.ctx.Err() == nil {
		batch, err := s.readCatchUp(sr)
		if err != nil || len(batch) == 0 {
			return
		}
		s.noteUSB(sr.wait)
		sr.wait = 0
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
						s.noteDecodeSend(primingPics)
						priming = nil
						primingPics = 0
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
		pics := pictureCount(show, view)
		if dec == nil {
			priming = append(priming, show...)
			primingPics += pics
			if len(priming) > 48 {
				return
			}
			continue
		}
		for _, blob := range show {
			writeLive(dec, blob)
		}
		s.noteDecodeSend(pics)
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

func (s *Session) noteUSB(d time.Duration) {
	s.mu.Lock()
	s.usbWait = d
	if d > s.usbMax {
		s.usbMax = d
	}
	s.mu.Unlock()
}

func (s *Session) noteDecodeSend(n int) {
	if n <= 0 {
		return
	}
	now := time.Now()
	s.mu.Lock()
	for i := 0; i < n; i++ {
		s.decQ = append(s.decQ, now)
	}
	if len(s.decQ) > 180 {
		s.decQ = append([]time.Time(nil), s.decQ[len(s.decQ)-180:]...)
	}
	s.sentN += n
	s.mu.Unlock()
}

func (s *Session) noteDecodeDone(shown bool) {
	now := time.Now()
	s.mu.Lock()
	if len(s.decQ) > 0 {
		s.decLast = now.Sub(s.decQ[0])
		s.decQ = s.decQ[1:]
	}
	if shown {
		s.frameAt = now
		if s.fpsT.IsZero() {
			s.fpsT = now
		}
		s.fpsN++
		if now.Sub(s.fpsT) >= time.Second {
			s.fps = s.fpsN
			s.fpsN = 0
			s.fpsT = now
		}
	}
	s.maybeLogLocked(now)
	s.mu.Unlock()
}

func (s *Session) maybeLogLocked(now time.Time) {
	if s.logClosed {
		return
	}
	if !s.logAt.IsZero() && now.Sub(s.logAt) < time.Second {
		return
	}
	s.logAt = now
	if s.logf == nil {
		s.openLogLocked()
	}
	if s.logf == nil {
		return
	}
	screen := time.Duration(0)
	if !s.frameAt.IsZero() {
		screen = now.Sub(s.frameAt)
	}
	path := "video"
	if s.tier == "capture" {
		path = "capture"
	}
	fmt.Fprintf(s.logf, "%s path=%s %dx%d fps=%d usb=%d usbMax=%d decode=%d screen=%d clock=%d queue=%d tier=%s enc=%s shot=%d sent=%d\n",
		now.Format("15:04:05"),
		path,
		s.videoW, s.videoH,
		s.fps,
		s.usbWait.Milliseconds(),
		s.usbMax.Milliseconds(),
		s.decLast.Milliseconds(),
		screen.Milliseconds(),
		s.lag.Milliseconds(),
		len(s.decQ),
		s.tier,
		s.encoder,
		s.shot.Milliseconds(),
		s.sentN,
	)
	s.usbMax = 0
	s.sentN = 0
}

func (s *Session) openLogLocked() {
	path := latencyLogPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	s.logf = f
	s.logPath = path
	fmt.Fprintf(f, "--- %s ---\n", time.Now().Format(time.RFC3339))
	_, _ = f.WriteString("# usb=PC wait for bytes (ms). usbMax=worst this second. decode=ffmpeg (ms). screen=picture age (ms). clock=PTS drift (ms; a steady 0 can still hide a constant delay inside the phone). queue=frames sitting in ffmpeg. shot=screencap (ms). sent=pictures given to ffmpeg this second.\n")
}

func latencyLogPath() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		base = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(base, "Luma", "latency.log")
}

func (s *Session) TraceLine() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tier == "capture" {
		return fmt.Sprintf("slow capture · shot %dms", s.shot.Milliseconds()), s.logPath
	}
	screen := time.Duration(0)
	if !s.frameAt.IsZero() {
		screen = time.Since(s.frameAt)
	}
	fps := s.fps
	if fps == 0 {
		fps = s.fpsN
	}
	tier := s.tier
	if tier == "" {
		tier = "video"
	}
	if s.encoder != "" {
		tier = tier + " " + s.encoder
	}
	line := fmt.Sprintf("%d fps · usb %dms · decode %dms · screen %dms · clock %dms · %s",
		fps, s.usbWait.Milliseconds(), s.decLast.Milliseconds(), screen.Milliseconds(), s.lag.Milliseconds(), tier)
	if len(s.decQ) > 2 {
		line += fmt.Sprintf(" · queue %d", len(s.decQ))
	}
	return line, s.logPath
}

func pictureCount(show [][]byte, view []viewPkt) int {
	n := 0
	for _, p := range view {
		if p.config || len(p.blob) == 0 {
			continue
		}
		for _, b := range show {
			if len(b) > 0 && &b[0] == &p.blob[0] {
				n++
				break
			}
		}
	}
	return n
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
	c    net.Conn
	buf  []byte
	wait time.Duration
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
	if len(s.buf) >= n {
		return nil
	}
	start := time.Now()
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
	s.wait = time.Since(start)
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
	var last error
	for _, filter := range []string{
		// Filler NALs are bitrate padding. Reorder fields exist only on newer
		// ffmpeg; a build that rejects them exits at once and the next filter is used.
		"h264_metadata=delete_filler=1:num_reorder_frames=0:max_dec_frame_buffering=1",
		"h264_metadata=delete_filler=1",
		"",
	} {
		in, err := s.spawnDecoder(w, h, filter)
		if err == nil {
			return in, nil
		}
		last = err
	}
	return nil, last
}

func (s *Session) spawnDecoder(w, h int, filter string) (io.WriteCloser, error) {
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
	if filter != "" {
		args = append(args, "-bsf:v", filter)
	}
	args = append(args,
		"-i", "pipe:0",
		"-an",
	)
	pw, ph := previewSize(w, h)
	if pw != w || ph != h {
		// The window shows the phone much smaller than 1080p. Decoding the
		// full RGBA makes the PC fall behind, which queues about half a
		// second and then skips pictures (the stepped look).
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d:flags=fast_bilinear", pw, ph))
	}
	args = append(args,
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
	go s.pumpRaw(stdout, pw, ph)
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
			s.noteDecodeDone(false)
		}
		if _, err := io.ReadFull(br, imgs[slot].Pix); err != nil {
			return
		}
		s.noteDecodeDone(true)
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
			t0 := time.Now()
			cmd := s.adb.ExecOut(s.ctx, s.serial, "screencap", "-p")
			out, err := cmd.Output()
			shot := time.Since(t0)
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
			s.shot = shot
			s.frameAt = time.Now()
			s.maybeLogLocked(s.frameAt)
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

func previewSize(w, h int) (int, int) {
	const maxLong = 1280
	if w < 2 || h < 2 {
		return w, h
	}
	long := w
	if h > long {
		long = h
	}
	if long <= maxLong {
		return evenDim(w), evenDim(h)
	}
	nw := evenDim(w * maxLong / long)
	nh := evenDim(h * maxLong / long)
	if nw < 2 {
		nw = 2
	}
	if nh < 2 {
		nh = 2
	}
	return nw, nh
}

func evenDim(n int) int {
	if n%2 != 0 {
		return n - 1
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
	// 256KB holds about a quarter second of 8Mbps video when the PC is
	// briefly late. A smaller window pushes back instead of hiding delay.
	_ = tcp.SetReadBuffer(64 * 1024)
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
