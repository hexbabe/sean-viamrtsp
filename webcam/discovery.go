package webcam

/*
#cgo CFLAGS: -Wno-deprecated-declarations
#include <libavformat/avformat.h>
#include <libavdevice/avdevice.h>
#include <libavutil/log.h>
#include <libavutil/opt.h>
#include <libavutil/pixdesc.h>
#include <libavutil/error.h>
#include <libavutil/dict.h>
#include <errno.h>
#include <stdlib.h>
#include <stdarg.h>
#include <stdio.h>

extern void goFFmpegLogBridge(int level, char* msg);

static void ffmpeg_log_bridge(void* ptr, int level, const char* fmt, va_list vl) {
	char message[1024];
	vsnprintf(message, sizeof(message), fmt, vl);
	goFFmpegLogBridge(level, message);
}

static void set_go_log_callback() {
	av_log_set_callback(ffmpeg_log_bridge);
}

static void reset_log_callback() {
	av_log_set_callback(av_log_default_callback);
}

static int av_error_enosys() {
	return AVERROR(ENOSYS);
}

static int av_error_exit() {
	return AVERROR_EXIT;
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/discovery"
)

var (
	// DiscoveryModel identifies the FFmpeg-based webcam discovery service.
	DiscoveryModel = resource.NewModel("viam", "viamrtsp", "webcam-discovery")
)

func init() {
	resource.RegisterService(
		discovery.API,
		DiscoveryModel,
		resource.Registration[discovery.Service, resource.NoNativeConfig]{
			Constructor: newFFmpegWebcamDiscovery,
		},
	)
}

type ffmpegWebcamDiscovery struct {
	resource.Named
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	logger logging.Logger
}

func newFFmpegWebcamDiscovery(
	_ context.Context,
	_ resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (discovery.Service, error) {
	return &ffmpegWebcamDiscovery{
		Named:  conf.ResourceName().AsNamed(),
		logger: logger,
	}, nil
}

func (s *ffmpegWebcamDiscovery) DiscoverResources(ctx context.Context, _ map[string]any) ([]resource.Config, error) {
	ensureDevicesRegistered()

	configs, err := discoverWebcams(ctx, s.logger)
	if err != nil {
		return nil, err
	}
	return configs, nil
}

type ffmpegDevice struct {
	Path        string
	Description string
	FormatName  string
}

type videoMode struct {
	Width       int
	Height      int
	FrameRate   float64
	PixelFormat string
}

func (vm videoMode) key() string {
	return fmt.Sprintf("%dx%d@%.3f/%s", vm.Width, vm.Height, math.Round(vm.FrameRate*1000)/1000, vm.PixelFormat)
}

func (vm videoMode) isValid() bool {
	return vm.Width > 0 && vm.Height > 0
}

func discoverWebcams(ctx context.Context, logger logging.Logger) ([]resource.Config, error) {
	formatName, err := inputFormatForRuntime()
	if err != nil {
		return nil, err
	}

	devices, err := listDevices(formatName, logger)
	if err != nil {
		return nil, err
	}

	if len(devices) == 0 {
		fallback := defaultDevicePath()
		if fallback == "" {
			return nil, errors.New("no webcams detected via ffmpeg")
		}
		devices = []ffmpegDevice{{Path: fallback, Description: fallback, FormatName: formatName}}
	}

	var configs []resource.Config
	for _, dev := range devices {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		modes := probeDeviceModes(dev, logger)
		if len(modes) == 0 {
			if fallbackMode, modeErr := fetchDefaultMode(dev); modeErr == nil && fallbackMode != nil && fallbackMode.isValid() {
				modes = append(modes, *fallbackMode)
			} else if modeErr != nil {
				logger.Debugw("unable to fetch default ffmpeg mode", "device", dev.Path, "error", modeErr)
			}
		}

		if len(modes) == 0 {
			logger.Debugw("skipping webcam with no detectable modes", "device", dev.Path)
			continue
		}

		configs = append(configs, buildWebcamConfigs(dev, modes)...)
	}

	if len(configs) == 0 {
		return nil, errors.New("no ffmpeg webcam configurations generated")
	}

	return configs, nil
}

func inputFormatForRuntime() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return "avfoundation", nil
	case "linux":
		return "v4l2", nil
	case "windows":
		return "dshow", nil
	default:
		return "", fmt.Errorf("webcam discovery unsupported on %s", runtime.GOOS)
	}
}

func defaultDevicePath() string {
	switch runtime.GOOS {
	case "darwin":
		return "0"
	case "linux":
		return "/dev/video0"
	case "windows":
		return "video=0"
	default:
		return ""
	}
}

func listDevices(formatName string, logger logging.Logger) ([]ffmpegDevice, error) {
	if runtime.GOOS == "darwin" {
		return listMacDevices(logger)
	}
	return listDevicesViaAPI(formatName)
}

func listDevicesViaAPI(formatName string) ([]ffmpegDevice, error) {
	cFormat := C.CString(formatName)
	defer C.free(unsafe.Pointer(cFormat))

	inputFmt := C.av_find_input_format(cFormat)
	if inputFmt == nil {
		return nil, fmt.Errorf("ffmpeg input format %s unavailable", formatName)
	}

	var deviceList *C.AVDeviceInfoList
	ret := C.avdevice_list_input_sources(inputFmt, nil, nil, &deviceList)
	if ret < 0 {
		if ret == C.av_error_enosys() {
			return []ffmpegDevice{}, nil
		}
		return nil, fmt.Errorf("ffmpeg could not list devices: %s", avErrorString(ret))
	}
	defer C.avdevice_free_list_devices(&deviceList)

	count := int(deviceList.nb_devices)
	if count == 0 {
		return []ffmpegDevice{}, nil
	}

	devPtrs := (*[1 << 30]*C.AVDeviceInfo)(unsafe.Pointer(deviceList.devices))[:count:count]
	devices := make([]ffmpegDevice, 0, count)
	for _, dev := range devPtrs {
		if dev == nil {
			continue
		}
		name := C.GoString(dev.device_name)
		desc := C.GoString(dev.device_description)
		if name == "" {
			name = desc
		}
		if name == "" {
			continue
		}
		devices = append(devices, ffmpegDevice{
			Path:        name,
			Description: desc,
			FormatName:  formatName,
		})
	}
	return devices, nil
}

var macDeviceLine = regexp.MustCompile(`^\[(\d+)\]\s+(.*)$`)

func listMacDevices(logger logging.Logger) ([]ffmpegDevice, error) {
	lines, err := withFFmpegLogCapture(func() error {
		return callFFmpegWithOptions("avfoundation", "", map[string]string{"list_devices": "true"})
	})
	if err != nil {
		logger.Debugw("ffmpeg list_devices call failed", "error", err)
	}

	var devices []ffmpegDevice
	inVideoSection := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case "AVFoundation video devices:":
			inVideoSection = true
			continue
		case "AVFoundation audio devices:":
			inVideoSection = false
			continue
		}

		if !inVideoSection {
			continue
		}

		matches := macDeviceLine.FindStringSubmatch(trimmed)
		if len(matches) != 3 {
			continue
		}
		index := matches[1]
		name := matches[2]
		if strings.HasPrefix(strings.ToLower(name), "capture screen") {
			continue
		}
		devices = append(devices, ffmpegDevice{
			Path:        index,
			Description: name,
			FormatName:  "avfoundation",
		})
	}

	return devices, nil
}

func probeDeviceModes(dev ffmpegDevice, logger logging.Logger) []videoMode {
	lines, err := withFFmpegLogCapture(func() error {
		switch runtime.GOOS {
		case "darwin":
			return callFFmpegWithOptions(dev.FormatName, dev.Path, map[string]string{
				"video_size": "9999x9999",
			})
		case "linux":
			return callFFmpegWithOptions(dev.FormatName, dev.Path, map[string]string{
				"list_formats": "all",
			})
		case "windows":
			return callFFmpegWithOptions(dev.FormatName, dev.Path, map[string]string{
				"list_options": "true",
			})
		default:
			return fmt.Errorf("unsupported runtime %s", runtime.GOOS)
		}
	})
	if err != nil {
		logger.Debugw("ffmpeg capability probe failed", "device", dev.Path, "error", err)
	}

	switch runtime.GOOS {
	case "darwin":
		return parseDarwinModes(lines)
	case "linux":
		return parseLinuxModes(lines)
	case "windows":
		return parseWindowsModes(lines)
	default:
		return nil
	}
}

var darwinModeRe = regexp.MustCompile(`(?m)\s+(\d+)x(\d+)@\[(\d+(?:\.\d+)?)\s+(\d+(?:\.\d+)?)\]fps`)

func parseDarwinModes(lines []string) []videoMode {
	var modes []videoMode
	for _, line := range lines {
		matches := darwinModeRe.FindStringSubmatch(line)
		if len(matches) != 5 {
			continue
		}
		width, _ := strconv.Atoi(matches[1])
		height, _ := strconv.Atoi(matches[2])
		minFPS, _ := strconv.ParseFloat(matches[3], 64)
		maxFPS, _ := strconv.ParseFloat(matches[4], 64)

		if math.Abs(maxFPS-minFPS) < 0.01 && minFPS > 0 {
			modes = append(modes, videoMode{Width: width, Height: height, FrameRate: minFPS})
		} else {
			modes = append(modes, videoMode{Width: width, Height: height})
		}
	}
	return modes
}

var sizeRe = regexp.MustCompile(`(\d+)x(\d+)`)
var linuxFormatRe = regexp.MustCompile(`^(Raw|Compressed):\s+([^:]+):`)

func parseLinuxModes(lines []string) []videoMode {
	var modes []videoMode
	currentFmt := ""

	for _, line := range lines {
		matches := linuxFormatRe.FindStringSubmatch(line)
		if len(matches) >= 3 {
			currentFmt = strings.TrimSpace(matches[2])
		}

		for _, size := range sizeRe.FindAllStringSubmatch(line, -1) {
			if len(size) != 3 {
				continue
			}
			width, _ := strconv.Atoi(size[1])
			height, _ := strconv.Atoi(size[2])
			modes = append(modes, videoMode{
				Width:       width,
				Height:      height,
				PixelFormat: currentFmt,
			})
		}
	}

	return modes
}

var (
	windowsRangeRe      = regexp.MustCompile(`min s=(\d+)x(\d+)\s+fps=([\d\.]+)\s+max s=(\d+)x(\d+)\s+fps=([\d\.]+)`)
	windowsPixelFmtRe   = regexp.MustCompile(`pixel_format=([A-Za-z0-9_]+)`)
	windowsCodecFmtRe   = regexp.MustCompile(`vcodec=([A-Za-z0-9_]+)`)
	windowsResolutionRe = regexp.MustCompile(`\b(\d+)x(\d+)\b`)
)

func parseWindowsModes(lines []string) []videoMode {
	var modes []videoMode
	currentFmt := ""

	for _, line := range lines {
		if match := windowsPixelFmtRe.FindStringSubmatch(line); len(match) == 2 {
			currentFmt = match[1]
		} else if match := windowsCodecFmtRe.FindStringSubmatch(line); len(match) == 2 {
			currentFmt = match[1]
		}

		if rangeMatch := windowsRangeRe.FindStringSubmatch(line); len(rangeMatch) == 7 {
			minW, _ := strconv.Atoi(rangeMatch[1])
			minH, _ := strconv.Atoi(rangeMatch[2])
			minFPS, _ := strconv.ParseFloat(rangeMatch[3], 64)
			maxW, _ := strconv.Atoi(rangeMatch[4])
			maxH, _ := strconv.Atoi(rangeMatch[5])
			maxFPS, _ := strconv.ParseFloat(rangeMatch[6], 64)

			if minFPS > 0 {
				modes = append(modes, videoMode{Width: minW, Height: minH, FrameRate: minFPS, PixelFormat: currentFmt})
			}
			if maxFPS > 0 && (maxW != minW || maxH != minH || math.Abs(maxFPS-minFPS) > 0.01) {
				modes = append(modes, videoMode{Width: maxW, Height: maxH, FrameRate: maxFPS, PixelFormat: currentFmt})
			}
			continue
		}

		resMatches := windowsResolutionRe.FindAllStringSubmatch(line, -1)
		for _, res := range resMatches {
			if len(res) != 3 {
				continue
			}
			width, _ := strconv.Atoi(res[1])
			height, _ := strconv.Atoi(res[2])
			modes = append(modes, videoMode{
				Width:       width,
				Height:      height,
				PixelFormat: currentFmt,
			})
		}
	}

	return modes
}

func fetchDefaultMode(dev ffmpegDevice) (*videoMode, error) {
	cFormat := C.CString(dev.FormatName)
	defer C.free(unsafe.Pointer(cFormat))

	inputFmt := C.av_find_input_format(cFormat)
	if inputFmt == nil {
		return nil, fmt.Errorf("ffmpeg input format %s unavailable", dev.FormatName)
	}

	devicePath := dev.Path
	if devicePath == "" {
		devicePath = defaultDevicePath()
	}

	cDevice := C.CString(devicePath)
	defer C.free(unsafe.Pointer(cDevice))

	var ctx *C.AVFormatContext
	if ret := C.avformat_open_input(&ctx, cDevice, inputFmt, nil); ret < 0 {
		return nil, fmt.Errorf("ffmpeg open failed: %s", avErrorString(ret))
	}
	defer C.avformat_close_input(&ctx)

	if ret := C.avformat_find_stream_info(ctx, nil); ret < 0 {
		return nil, fmt.Errorf("ffmpeg stream info failed: %s", avErrorString(ret))
	}

	var stream *C.AVStream
	for i := C.uint(0); i < ctx.nb_streams; i++ {
		elem := *(**C.AVStream)(unsafe.Pointer(uintptr(unsafe.Pointer(ctx.streams)) + uintptr(i)*unsafe.Sizeof((*C.AVStream)(nil))))
		if elem.codecpar.codec_type == C.AVMEDIA_TYPE_VIDEO {
			stream = elem
			break
		}
	}

	if stream == nil {
		return nil, errors.New("no video stream detected")
	}

	fps := rationalToFloat(stream.r_frame_rate)
	if fps == 0 {
		fps = rationalToFloat(stream.avg_frame_rate)
	}

	mode := &videoMode{
		Width:     int(stream.codecpar.width),
		Height:    int(stream.codecpar.height),
		FrameRate: fps,
	}

	if stream.codecpar.format >= 0 {
		pixFmt := C.av_get_pix_fmt_name((C.enum_AVPixelFormat)(stream.codecpar.format))
		if pixFmt != nil {
			mode.PixelFormat = C.GoString(pixFmt)
		}
	}

	return mode, nil
}

func rationalToFloat(r C.AVRational) float64 {
	if r.den == 0 {
		return 0
	}
	return float64(r.num) / float64(r.den)
}

func buildWebcamConfigs(dev ffmpegDevice, modes []videoMode) []resource.Config {
	deduped := dedupeModes(modes)
	if len(deduped) == 0 {
		return nil
	}

	sort.Slice(deduped, func(i, j int) bool {
		if deduped[i].Width != deduped[j].Width {
			return deduped[i].Width < deduped[j].Width
		}
		if deduped[i].Height != deduped[j].Height {
			return deduped[i].Height < deduped[j].Height
		}
		if math.Abs(deduped[i].FrameRate-deduped[j].FrameRate) > 0.01 {
			return deduped[i].FrameRate < deduped[j].FrameRate
		}
		return deduped[i].PixelFormat < deduped[j].PixelFormat
	})

	baseName := sanitizeName(dev.Description)
	if baseName == "" {
		baseName = sanitizeName(dev.Path)
	}
	if baseName == "" {
		baseName = "ffmpeg-webcam"
	}

	var configs []resource.Config

	for idx, mode := range deduped {
		if !mode.isValid() {
			continue
		}

		cfg := &Config{
			VideoPath: dev.Path,
			Format:    mode.PixelFormat,
			WidthPx:   mode.Width,
			HeightPx:  mode.Height,
		}
		if mode.FrameRate > 0 {
			cfg.FrameRate = math.Round(mode.FrameRate*1000) / 1000
		}

		attrs, err := toAttributesMap(cfg)
		if err != nil {
			continue
		}

		name := baseName
		if len(deduped) > 1 {
			name = fmt.Sprintf("%s-%d", baseName, idx)
		}

		configs = append(configs, resource.Config{
			Name:                name,
			API:                 camera.API,
			Model:               Model,
			Attributes:          attrs,
			ConvertedAttributes: cfg,
		})
	}

	return configs
}

func dedupeModes(modes []videoMode) []videoMode {
	seen := make(map[string]struct{})
	var result []videoMode
	for _, mode := range modes {
		if !mode.isValid() {
			continue
		}
		key := mode.key()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, mode)
	}
	return result
}

func sanitizeName(in string) string {
	const maxLen = 48
	builder := strings.Builder{}
	in = strings.ToLower(in)
	for _, r := range in {
		if builder.Len() >= maxLen {
			break
		}
		switch {
		case r >= 'a' && r <= 'z':
			fallthrough
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '-' || r == '_':
			builder.WriteRune(r)
		}
	}
	return strings.Trim(builder.String(), "-_")
}

func toAttributesMap(cfg *Config) (map[string]any, error) {
	bytes, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(bytes, &result); err != nil {
		return nil, err
	}
	return result, nil
}

var (
	logCapturePtr atomic.Pointer[logCapture]
	logCaptureMu  sync.Mutex
)

type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (lc *logCapture) append(line string) {
	trimmed := strings.TrimRight(line, "\r\n")
	if trimmed == "" {
		return
	}
	lc.mu.Lock()
	lc.lines = append(lc.lines, trimmed)
	lc.mu.Unlock()
}

//export goFFmpegLogBridge
func goFFmpegLogBridge(level C.int, msg *C.char) {
	_ = level
	capture := logCapturePtr.Load()
	if capture == nil || msg == nil {
		return
	}
	capture.append(C.GoString(msg))
}

func withFFmpegLogCapture(fn func() error) ([]string, error) {
	logCaptureMu.Lock()
	defer logCaptureMu.Unlock()

	capture := &logCapture{}
	logCapturePtr.Store(capture)

	prevLevel := C.av_log_get_level()
	C.set_go_log_callback()
	C.av_log_set_level(C.AV_LOG_INFO)

	defer func() {
		C.reset_log_callback()
		C.av_log_set_level(prevLevel)
		logCapturePtr.Store(nil)
	}()

	err := fn()
	return capture.lines, err
}

func callFFmpegWithOptions(formatName, devicePath string, options map[string]string) error {
	cFormat := C.CString(formatName)
	defer C.free(unsafe.Pointer(cFormat))

	inputFmt := C.av_find_input_format(cFormat)
	if inputFmt == nil {
		return fmt.Errorf("ffmpeg input format %s unavailable", formatName)
	}

	path := devicePath
	if path == "" {
		path = ""
	}
	cDevice := C.CString(path)
	defer C.free(unsafe.Pointer(cDevice))

	var dict *C.AVDictionary
	for key, value := range options {
		cKey := C.CString(key)
		cValue := C.CString(value)
		C.av_dict_set(&dict, cKey, cValue, 0)
		C.free(unsafe.Pointer(cKey))
		C.free(unsafe.Pointer(cValue))
	}
	defer C.av_dict_free(&dict)

	var ctx *C.AVFormatContext
	ret := C.avformat_open_input(&ctx, cDevice, inputFmt, &dict)
	if ret >= 0 {
		C.avformat_close_input(&ctx)
		return nil
	}
	if ret == C.av_error_exit() {
		return nil
	}
	C.avformat_close_input(&ctx)
	return fmt.Errorf("ffmpeg returned %s", avErrorString(ret))
}

func avErrorString(code C.int) string {
	var buf [256]C.char
	if C.av_strerror(code, &buf[0], C.size_t(len(buf))) < 0 {
		return fmt.Sprintf("code %d", int(code))
	}
	return C.GoString(&buf[0])
}
