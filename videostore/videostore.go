// Package videostore stores video and allows it to be queried
package videostore

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"

	vscamera "github.com/viam-modules/video-store/model/camera"
	"github.com/viam-modules/video-store/videostore"
	vsutils "github.com/viam-modules/video-store/videostore/utils"
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

const (
	maxGRPCSize         = 1024 * 1024 * 32 // bytes
	defaultFramerate    = 20               // frames per second
	defaultVideoBitrate = 1000000
	defaultVideoPreset  = "ultrafast"
)

var (
	defaultStoragePath = filepath.Join(".viam", "video-storage")
	defaultUploadPath  = filepath.Join(".viam", "capture", "video-upload")
)

// Model is videostore's Viam model.
var Model = resource.ModelNamespace("viam").WithFamily("viamrtsp").WithModel("video-store")

func init() {
	resource.RegisterComponent(generic.API, Model, resource.Registration[resource.Resource, *Config]{
		Constructor: New,
	})
}

type service struct {
	resource.AlwaysRebuild
	name   resource.Name
	logger logging.Logger
	vs     videostore.VideoStore
	rsMux  *rawSegmenterMux
}

// New creates a new videostore.
func New(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (resource.Resource, error) {
	newConf, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		logger.Error(err.Error())
		return nil, err
	}
	vsConfig, err := applyDefaults(newConf, conf.ResourceName().Name)
	if err != nil {
		return nil, err
	}
	var vs videostore.VideoStore
	var mux *rawSegmenterMux
	if newConf.Camera != nil {
		c, err := camera.FromDependencies(deps, *newConf.Camera)
		if err != nil {
			return nil, err
		}
		rtpVs, err := videostore.NewRTPVideoStore(ctx, videostore.Config{
			Name:    vsConfig.Name,
			Type:    videostore.SourceTypeRTP,
			Storage: vsConfig.Storage,
		}, logger)
		if err != nil {
			return nil, err
		}

		mux = newRawSegmenterMux(rtpVs.Segmenter(), c.Name(), logger)
		if err := mux.init(); err == nil {
			vs = rtpVs
		} else {
			rtpVs.Close()
			vsConfig.FramePoller.Camera = c
			fVs, err := videostore.NewFramePollingVideoStore(ctx, videostore.Config{
				Name:        vsConfig.Name,
				Type:        videostore.SourceTypeFrame,
				Storage:     vsConfig.Storage,
				Encoder:     vsConfig.Encoder,
				FramePoller: vsConfig.FramePoller,
			}, logger)
			if err != nil {
				return nil, err
			}
			vs = fVs
		}
	} else {
		vs, err = videostore.NewReadOnlyVideoStore(ctx, videostore.Config{
			Name:    vsConfig.Name,
			Type:    videostore.SourceTypeReadOnly,
			Storage: vsConfig.Storage,
		}, logger)
		if err != nil {
			return nil, err
		}
	}

	s := &service{
		name:   conf.ResourceName(),
		logger: logger,
		vs:     vs,
		rsMux:  mux,
	}
	return s, nil
}

func (s *service) Name() resource.Name {
	return s.name
}

func (s *service) Close(_ context.Context) error {
	if err := s.rsMux.close(); err != nil {
		return err
	}
	s.vs.Close()
	return nil
}

func (s *service) DoCommand(ctx context.Context, command map[string]interface{}) (map[string]interface{}, error) {
	cmd, ok := command["command"].(string)
	if !ok {
		return nil, errors.New("invalid command type")
	}

	switch cmd {
	// Save command is used to concatenate video clips between the given timestamps.
	// The concatenated video file is then uploaded to the cloud the upload path.
	// The response contains the name of the uploaded file.
	case "save":
		s.logger.Debug("save command received")
		req, err := toSaveCommand(command)
		if err != nil {
			return nil, err
		}

		res, err := s.vs.Save(ctx, req)
		if err != nil {
			return nil, err
		}

		ret := map[string]interface{}{
			"command":  "save",
			"filename": res.Filename,
		}

		if req.Async {
			ret["status"] = "async"
		}
		return ret, nil
	case "fetch":
		s.logger.Debug("fetch command received")
		req, err := toFetchCommand(command)
		if err != nil {
			return nil, err
		}
		res, err := s.vs.Fetch(ctx, req)
		if err != nil {
			return nil, err
		}
		if len(res.Video) > maxGRPCSize {
			return nil, errors.New("video file size exceeds max grpc size")
		}
		// TODO(seanp): Do we need to encode the video bytes to base64?
		videoBytesBase64 := base64.StdEncoding.EncodeToString(res.Video)
		return map[string]interface{}{
			"command": "fetch",
			"video":   videoBytesBase64,
		}, nil
	case "get-storage-state":
		s.logger.Debug("get-storage-state command received")
		state, err := s.vs.GetStorageState(ctx)
		if err != nil {
			return nil, err
		}
		return vscamera.GetStorageStateDoCommandResponse(state), nil
	default:
		return nil, errors.New("invalid command")
	}
}

func toSaveCommand(command map[string]interface{}) (*videostore.SaveRequest, error) {
	fromStr, ok := command["from"].(string)
	if !ok {
		return nil, errors.New("from timestamp not found")
	}
	from, err := vsutils.ParseDateTimeString(fromStr)
	if err != nil {
		return nil, err
	}
	toStr, ok := command["to"].(string)
	if !ok {
		return nil, errors.New("to timestamp not found")
	}
	to, err := vsutils.ParseDateTimeString(toStr)
	if err != nil {
		return nil, err
	}
	metadata, ok := command["metadata"].(string)
	if !ok {
		metadata = ""
	}
	async, ok := command["async"].(bool)
	if !ok {
		async = false
	}
	return &videostore.SaveRequest{
		From:     from,
		To:       to,
		Metadata: metadata,
		Async:    async,
	}, nil
}

func toFetchCommand(command map[string]interface{}) (*videostore.FetchRequest, error) {
	fromStr, ok := command["from"].(string)
	if !ok {
		return nil, errors.New("from timestamp not found")
	}
	from, err := vsutils.ParseDateTimeString(fromStr)
	if err != nil {
		return nil, err
	}
	toStr, ok := command["to"].(string)
	if !ok {
		return nil, errors.New("to timestamp not found")
	}
	to, err := vsutils.ParseDateTimeString(toStr)
	if err != nil {
		return nil, err
	}
	return &videostore.FetchRequest{From: from, To: to}, nil
}
