package webcam

/*
#include <libavformat/avformat.h>
#include <libavcodec/avcodec.h>
#include <libavdevice/avdevice.h>
#include <libavutil/imgutils.h>
#include <libavutil/opt.h>
#include <libavutil/error.h>
#include <libswscale/swscale.h>
#include <stdlib.h>
#include <errno.h>
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/pointcloud"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/rimage"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/utils"
)

var (
	// Model defines the webcam model
	Model = resource.ModelNamespace("viam").WithFamily("viamrtsp").WithModel("webcam")
)

var devicesRegistered bool

func init() {
	resource.RegisterComponent(camera.API, Model, resource.Registration[camera.Camera, *Config]{
		Constructor: NewWebcamCamera,
	})
}

// ensureDevicesRegistered registers all libavdevice formats once
func ensureDevicesRegistered() {
	if !devicesRegistered {
		C.avdevice_register_all()
		devicesRegistered = true
	}
}

// Config defines the configuration for a webcam camera
type Config struct {
	VideoPath string  `json:"video_path,omitempty"`
	Format    string  `json:"format,omitempty"`
	WidthPx   int     `json:"width_px,omitempty"`
	HeightPx  int     `json:"height_px,omitempty"`
	FrameRate float64 `json:"frame_rate,omitempty"`
}

// Validate validates the config
func (conf *Config) Validate(path string) ([]string, []string, error) {
	// Set default video path based on OS if not provided
	if conf.VideoPath == "" {
		switch runtime.GOOS {
		case "darwin": // macOS
			conf.VideoPath = "0"
		case "linux":
			conf.VideoPath = "/dev/video0"
		case "windows":
			conf.VideoPath = "video=0"
		}
	}

	return nil, nil, nil
}

// webcamCamera implements the camera.Camera interface for webcams
type webcamCamera struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name   resource.Name
	logger logging.Logger

	mu             sync.RWMutex
	formatCtx      *C.AVFormatContext
	codecCtx       *C.AVCodecContext
	swsCtx         *C.struct_SwsContext
	videoStreamIdx C.int
	cancelCtx      context.Context
	cancelFunc     context.CancelFunc
	latestFrame    image.Image
	activeWorker   sync.WaitGroup
	conf           *Config
}

// NewWebcamCamera creates a new webcam camera
func NewWebcamCamera(
	ctx context.Context,
	deps resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (camera.Camera, error) {
	newConf, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}

	cancelCtx, cancel := context.WithCancel(context.Background())

	wc := &webcamCamera{
		name:           conf.ResourceName(),
		logger:         logger,
		cancelCtx:      cancelCtx,
		cancelFunc:     cancel,
		videoStreamIdx: -1,
		conf:           newConf,
	}

	// Open the webcam device
	if err := wc.openDevice(newConf); err != nil {
		cancel()
		return nil, err
	}

	// Start background worker to read frames and handle reconnects
	wc.activeWorker.Add(1)
	utils.ManagedGo(wc.backgroundReader, wc.activeWorker.Done)

	return wc, nil
}

// openDevice opens the video device using libavformat/libavdevice. If opening with the configured
// frame rate fails (which can happen when the discovery-provided frame rate is slightly outside the
// range the driver actually accepts), we retry with a set of fallback frame rates (including the
// device defaults) so FFmpeg can choose a combination the hardware supports.
func (wc *webcamCamera) openDevice(conf *Config) error {
	originalConf := *conf
	candidates := frameRateCandidates(runtime.GOOS, conf.FrameRate)
	var lastErr error

	for idx, fr := range candidates {
		tempConf := originalConf
		tempConf.FrameRate = fr

		if idx > 0 {
			wc.logger.Infow("retrying webcam open with alternate frame rate",
				"frame_rate", fr)
		}

		if err := wc.openDeviceWithConfig(&tempConf); err == nil {
			*conf = tempConf
			return nil
		} else {
			lastErr = err
			wc.mu.Lock()
			wc.closeResources()
			wc.mu.Unlock()
		}
	}

	return lastErr
}

func (wc *webcamCamera) openDeviceWithConfig(conf *Config) error {
	// Ensure devices are registered
	ensureDevicesRegistered()

	// Determine input format and device path based on OS
	var inputFormatName *C.char
	var devicePath *C.char
	var defaultDevice string

	switch runtime.GOOS {
	case "darwin": // macOS
		inputFormatName = C.CString("avfoundation")
		defaultDevice = "0" // Default to first camera (index 0)
	case "linux":
		inputFormatName = C.CString("v4l2")
		defaultDevice = "/dev/video0"
	case "windows":
		inputFormatName = C.CString("dshow")
		defaultDevice = "video=0"
	default:
		return fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}
	defer C.free(unsafe.Pointer(inputFormatName))

	// Set device path
	if conf.VideoPath != "" {
		devicePath = C.CString(conf.VideoPath)
	} else {
		devicePath = C.CString(defaultDevice)
	}
	defer C.free(unsafe.Pointer(devicePath))

	wc.logger.Infof("Opening webcam with format=%s, device=%s", C.GoString(inputFormatName), C.GoString(devicePath))

	// Find input format
	inputFormat := C.av_find_input_format(inputFormatName)
	if inputFormat == nil {
		wc.logger.Errorf("Could not find input format: %s. Make sure libavdevice is properly installed and compiled with support for this format.", C.GoString(inputFormatName))
		wc.logger.Errorf("On macOS, you may need to install ffmpeg with: brew install ffmpeg")
		return fmt.Errorf("could not find input format: %s (libavdevice may not be compiled with support for this device type)", C.GoString(inputFormatName))
	}

	wc.logger.Debugf("Found input format: %s", C.GoString(inputFormat.name))

	// Set format options
	var options *C.AVDictionary
	defer C.av_dict_free(&options)

	// Set video size if specified
	if conf.WidthPx > 0 && conf.HeightPx > 0 {
		videoSize := fmt.Sprintf("%dx%d", conf.WidthPx, conf.HeightPx)
		videoSizeKey := C.CString("video_size")
		videoSizeVal := C.CString(videoSize)
		C.av_dict_set(&options, videoSizeKey, videoSizeVal, 0)
		C.free(unsafe.Pointer(videoSizeKey))
		C.free(unsafe.Pointer(videoSizeVal))
	}

	// Set frame rate if specified
	if conf.FrameRate > 0 {
		frameRate := fmt.Sprintf("%.2f", conf.FrameRate)
		frameRateKey := C.CString("framerate")
		frameRateVal := C.CString(frameRate)
		C.av_dict_set(&options, frameRateKey, frameRateVal, 0)
		C.free(unsafe.Pointer(frameRateKey))
		C.free(unsafe.Pointer(frameRateVal))
	}

	// Set pixel format if specified
	if conf.Format != "" {
		pixFmtKey := C.CString("pixel_format")
		pixFmtVal := C.CString(conf.Format)
		C.av_dict_set(&options, pixFmtKey, pixFmtVal, 0)
		C.free(unsafe.Pointer(pixFmtKey))
		C.free(unsafe.Pointer(pixFmtVal))
	}

	// Open input device
	wc.formatCtx = C.avformat_alloc_context()
	if wc.formatCtx == nil {
		return errors.New("could not allocate format context")
	}

	ret := C.avformat_open_input(&wc.formatCtx, devicePath, inputFormat, &options)
	if ret < 0 {
		var errbuf [1024]C.char
		C.av_strerror(ret, &errbuf[0], 1024)
		errStr := C.GoString(&errbuf[0])

		wc.logger.Errorf("Failed to open device '%s' with format '%s': %s (code: %d)",
			C.GoString(devicePath), C.GoString(inputFormatName), errStr, ret)

		return fmt.Errorf("could not open input device '%s': %s (error code: %d)", conf.VideoPath, errStr, ret)
	}

	// Retrieve stream information
	ret = C.avformat_find_stream_info(wc.formatCtx, nil)
	if ret < 0 {
		C.avformat_close_input(&wc.formatCtx)
		return fmt.Errorf("could not find stream information (error code: %d)", ret)
	}

	// Find the video stream
	for i := C.uint(0); i < wc.formatCtx.nb_streams; i++ {
		stream := *(**C.AVStream)(unsafe.Pointer(uintptr(unsafe.Pointer(wc.formatCtx.streams)) + uintptr(i)*unsafe.Sizeof((*C.AVStream)(nil))))
		if stream.codecpar.codec_type == C.AVMEDIA_TYPE_VIDEO {
			wc.videoStreamIdx = C.int(i)
			break
		}
	}

	if wc.videoStreamIdx == -1 {
		C.avformat_close_input(&wc.formatCtx)
		return errors.New("could not find video stream")
	}

	// Get codec parameters from the video stream
	videoStream := *(**C.AVStream)(unsafe.Pointer(uintptr(unsafe.Pointer(wc.formatCtx.streams)) + uintptr(wc.videoStreamIdx)*unsafe.Sizeof((*C.AVStream)(nil))))
	codecPar := videoStream.codecpar

	// Find decoder for the video stream
	codec := C.avcodec_find_decoder(codecPar.codec_id)
	if codec == nil {
		C.avformat_close_input(&wc.formatCtx)
		return errors.New("could not find codec")
	}

	// Allocate codec context
	wc.codecCtx = C.avcodec_alloc_context3(codec)
	if wc.codecCtx == nil {
		C.avformat_close_input(&wc.formatCtx)
		return errors.New("could not allocate codec context")
	}

	// Copy codec parameters to codec context
	ret = C.avcodec_parameters_to_context(wc.codecCtx, codecPar)
	if ret < 0 {
		C.avcodec_free_context(&wc.codecCtx)
		C.avformat_close_input(&wc.formatCtx)
		return fmt.Errorf("could not copy codec parameters (error code: %d)", ret)
	}

	// Open codec
	ret = C.avcodec_open2(wc.codecCtx, codec, nil)
	if ret < 0 {
		C.avcodec_free_context(&wc.codecCtx)
		C.avformat_close_input(&wc.formatCtx)
		return fmt.Errorf("could not open codec (error code: %d)", ret)
	}

	wc.logger.Infof("Opened webcam: %s, resolution: %dx%d, format: %d",
		conf.VideoPath, wc.codecCtx.width, wc.codecCtx.height, wc.codecCtx.pix_fmt)

	return nil
}

func frameRateCandidates(goos string, configured float64) []float64 {
	var candidates []float64
	seen := map[string]struct{}{}

	add := func(value float64) {
		if value < 0 {
			return
		}
		key := fmt.Sprintf("%.6f", value)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, value)
	}

	if configured >= 0 {
		add(configured)
	}

	if goos == "darwin" {
		if configured > 0 {
			rounded := math.Round(configured)
			if math.Abs(rounded-configured) > 0.01 {
				add(rounded)
			}
		}
		for _, fr := range []float64{60, 59.94, 50, 30, 29.97, 25, 24, 20, 15, 10, 5, 1} {
			add(fr)
		}
	}

	add(0)
	return candidates
}

// readFrames continuously reads frames from the webcam. It returns an error if the
// stream is interrupted and cannot recover.
func (wc *webcamCamera) readFrames() error {
	packet := C.av_packet_alloc()
	if packet == nil {
		return errors.New("could not allocate packet")
	}
	defer C.av_packet_free(&packet)

	frame := C.av_frame_alloc()
	if frame == nil {
		return errors.New("could not allocate frame")
	}
	defer C.av_frame_free(&frame)

	// Allocate frame for RGB conversion
	rgbFrame := C.av_frame_alloc()
	if rgbFrame == nil {
		return errors.New("could not allocate RGB frame")
	}
	defer C.av_frame_free(&rgbFrame)

	for {
		if wc.cancelCtx.Err() != nil {
			return nil
		}

		// Read frame from device
		ret := C.av_read_frame(wc.formatCtx, packet)
		if ret < 0 {
			// Some errors are recoverable, but most mean the device is disconnected
			// or in a bad state.
			if ret == -C.EAGAIN {
				continue
			}
			var errbuf [1024]C.char
			C.av_strerror(ret, &errbuf[0], 1024)
			errStr := C.GoString(&errbuf[0])
			return fmt.Errorf("error reading frame: %s (error code: %d)", errStr, ret)
		}

		// Check if this packet belongs to the video stream
		if packet.stream_index != wc.videoStreamIdx {
			C.av_packet_unref(packet)
			continue
		}

		// Send packet to decoder
		ret = C.avcodec_send_packet(wc.codecCtx, packet)
		if ret < 0 {
			wc.logger.Debugw("error sending packet to decoder", "error_code", ret)
			C.av_packet_unref(packet)
			continue
		}

		// Receive decoded frame
		ret = C.avcodec_receive_frame(wc.codecCtx, frame)
		if ret < 0 {
			C.av_packet_unref(packet)
			continue
		}

		// Convert frame to Go image
		img := wc.frameToImage(frame, rgbFrame)
		if img != nil {
			wc.mu.Lock()
			wc.latestFrame = img
			wc.mu.Unlock()
		}

		C.av_packet_unref(packet)
	}
}

// backgroundReader manages the frame reading loop and handles reconnection.
func (wc *webcamCamera) backgroundReader() {
	for wc.cancelCtx.Err() == nil {
		err := wc.readFrames()
		if err == nil {
			// Normal exit from cancellation.
			return
		}
		wc.logger.Warnw("frame reading loop stopped with error, will attempt to reconnect", "error", err)

		wc.mu.Lock()
		wc.closeResources()
		wc.mu.Unlock()

		reconnectSleep := 1 * time.Second
		for wc.cancelCtx.Err() == nil {
			wc.logger.Info("attempting to reopen webcam device")
			if err := wc.openDevice(wc.conf); err == nil {
				wc.logger.Info("successfully reopened webcam device")
				break // Reconnected, break back to the frame reading loop.
			}

			select {
			case <-wc.cancelCtx.Done():
				return
			case <-time.After(reconnectSleep):
				if reconnectSleep < 15*time.Second {
					reconnectSleep *= 2
				}
			}
		}
	}
}

// frameToImage converts an AVFrame to a Go image.Image
func (wc *webcamCamera) frameToImage(frame *C.AVFrame, rgbFrame *C.AVFrame) image.Image {
	// If the frame is already in YUV420P format, convert directly to Go image
	if frame.format == C.AV_PIX_FMT_YUV420P || frame.format == C.AV_PIX_FMT_YUVJ420P {
		return wc.yuv420pToImage(frame)
	}

	// Otherwise, convert to YUV420P using swscale
	wc.mu.Lock()
	defer wc.mu.Unlock()

	// (Re)create sws context if needed
	if wc.swsCtx == nil {
		wc.swsCtx = C.sws_getContext(
			frame.width, frame.height, (C.enum_AVPixelFormat)(frame.format),
			frame.width, frame.height, C.AV_PIX_FMT_YUV420P,
			C.SWS_FAST_BILINEAR, nil, nil, nil,
		)
		if wc.swsCtx == nil {
			wc.logger.Error("could not create swscale context")
			return nil
		}
	}

	// Prepare RGB frame
	if rgbFrame.width != frame.width || rgbFrame.height != frame.height {
		rgbFrame.format = C.int(C.AV_PIX_FMT_YUV420P)
		rgbFrame.width = frame.width
		rgbFrame.height = frame.height
		ret := C.av_frame_get_buffer(rgbFrame, 32)
		if ret < 0 {
			wc.logger.Debugw("could not allocate frame buffer", "error_code", ret)
			return nil
		}
	}

	// Convert frame
	ret := C.sws_scale(
		wc.swsCtx,
		(**C.uint8_t)(unsafe.Pointer(&frame.data[0])),
		(*C.int)(unsafe.Pointer(&frame.linesize[0])),
		0,
		frame.height,
		(**C.uint8_t)(unsafe.Pointer(&rgbFrame.data[0])),
		(*C.int)(unsafe.Pointer(&rgbFrame.linesize[0])),
	)
	if ret < 0 {
		wc.logger.Debugw("error converting frame", "error_code", ret)
		return nil
	}

	return wc.yuv420pToImage(rgbFrame)
}

// yuv420pToImage converts a YUV420P AVFrame to a Go image.YCbCr
func (wc *webcamCamera) yuv420pToImage(frame *C.AVFrame) image.Image {
	width := int(frame.width)
	height := int(frame.height)
	if width <= 0 || height <= 0 {
		return nil
	}

	yStride := int(frame.linesize[0])
	cStride := int(frame.linesize[1])

	yPlaneSize := yStride * height
	cPlaneSize := cStride * (height / 2)

	yDataPtr := unsafe.Pointer(frame.data[0])
	ySlice := (*[1 << 30]byte)(yDataPtr)[:yPlaneSize:yPlaneSize]

	cbDataPtr := unsafe.Pointer(frame.data[1])
	cbSlice := (*[1 << 30]byte)(cbDataPtr)[:cPlaneSize:cPlaneSize]

	crDataPtr := unsafe.Pointer(frame.data[2])
	crSlice := (*[1 << 30]byte)(crDataPtr)[:cPlaneSize:cPlaneSize]

	return &image.YCbCr{
		Y:              ySlice,
		Cb:             cbSlice,
		Cr:             crSlice,
		YStride:        yStride,
		CStride:        cStride,
		SubsampleRatio: image.YCbCrSubsampleRatio420,
		Rect:           image.Rect(0, 0, width, height),
	}
}

// Image returns a single image from the webcam
func (wc *webcamCamera) Image(ctx context.Context, mimeType string, extra map[string]interface{}) ([]byte, camera.ImageMetadata, error) {
	wc.mu.RLock()
	frame := wc.latestFrame
	wc.mu.RUnlock()

	if frame == nil {
		return nil, camera.ImageMetadata{}, errors.New("no frame available")
	}

	// Encode the image using rimage
	outBytes, err := rimage.EncodeImage(ctx, frame, mimeType)
	if err != nil {
		return nil, camera.ImageMetadata{}, err
	}

	return outBytes, camera.ImageMetadata{MimeType: mimeType}, nil
}

// Images returns the next image from the webcam
func (wc *webcamCamera) Images(ctx context.Context, filterSourceNames []string, extra map[string]interface{}) ([]camera.NamedImage, resource.ResponseMetadata, error) {
	wc.mu.RLock()
	frame := wc.latestFrame
	wc.mu.RUnlock()

	if frame == nil {
		return nil, resource.ResponseMetadata{}, errors.New("no frame available")
	}

	// Create NamedImage from the image
	namedImage, err := camera.NamedImageFromImage(frame, wc.name.String(), "image/jpeg")
	if err != nil {
		return nil, resource.ResponseMetadata{}, err
	}

	return []camera.NamedImage{namedImage}, resource.ResponseMetadata{}, nil
}

// NextPointCloud is not supported
func (wc *webcamCamera) NextPointCloud(ctx context.Context) (pointcloud.PointCloud, error) {
	return nil, errors.New("not implemented")
}

// Properties returns the camera properties
func (wc *webcamCamera) Properties(ctx context.Context) (camera.Properties, error) {
	return camera.Properties{
		SupportsPCD: false,
	}, nil
}

// Close closes the webcam
func (wc *webcamCamera) Close(ctx context.Context) error {
	wc.cancelFunc()
	wc.activeWorker.Wait()

	wc.mu.Lock()
	defer wc.mu.Unlock()

	wc.closeResources()
	return nil
}

// closeResources closes all C-level resources. Must be called with wc.mu locked.
func (wc *webcamCamera) closeResources() {
	if wc.swsCtx != nil {
		C.sws_freeContext(wc.swsCtx)
		wc.swsCtx = nil
	}

	if wc.codecCtx != nil {
		C.avcodec_free_context(&wc.codecCtx)
		wc.codecCtx = nil
	}

	if wc.formatCtx != nil {
		C.avformat_close_input(&wc.formatCtx)
		wc.formatCtx = nil
	}
}

// DoCommand allows executing custom commands
func (wc *webcamCamera) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return nil, fmt.Errorf("not implemented")
}

// Name returns the resource name
func (wc *webcamCamera) Name() resource.Name {
	return wc.name
}

func (c *webcamCamera) Geometries(ctx context.Context, extra map[string]interface{}) ([]spatialmath.Geometry, error) {
	return make([]spatialmath.Geometry, 0), nil
}
