// viamonvif/encoder.go
package viamonvif

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/use-go/onvif"
	"github.com/use-go/onvif/media"
	"github.com/use-go/onvif/xsd"
	onvifXsd "github.com/use-go/onvif/xsd/onvif"
	"go.viam.com/rdk/logging"
)

// Encoder is responsible for modifying the video encoder settings of an ONVIF-compatible camera.
type Encoder struct {
    rtspAddress string
    logger      logging.Logger
}

// NewEncoder creates a new Encoder instance with the provided logger.
func NewEncoder(rtspAddress string, logger logging.Logger) *Encoder {
    return &Encoder{
        rtspAddress: rtspAddress,
        logger:      logger,
    }
}

// SetResolution adjusts the resolution of the camera to the specified width and height.
func (e *Encoder) SetResolution(width, height int) error {
    baseAddress, err := extractBaseAddressFromRTSP(e.rtspAddress)
    if err != nil {
        return fmt.Errorf("failed to extract base address from RTSP address: %w", err)
    }

    // Create a new ONVIF device
    deviceInstance, err := onvif.NewDevice(onvif.DeviceParams{
        Xaddr:      baseAddress,
        Username:   "admin",
        Password:   "admin",
        HttpClient: &http.Client{Timeout: 5 * time.Second},
    })
    if err != nil {
        return fmt.Errorf("failed to create ONVIF device: %w", err)
    }

    // Get the available media profiles
    profiles, err := getMediaProfiles(deviceInstance)
    if err != nil {
        return fmt.Errorf("failed to get media profiles: %w", err)
    }

    // Iterate over profiles and update the resolution
    for _, profile := range profiles {
        err := setVideoEncoderConfiguration(deviceInstance, profile, width, height)
        if err != nil {
            e.logger.Warnf("Failed to set resolution for profile %s: %v", profile.Name, err)
            continue
        }
        e.logger.Infof("Successfully updated resolution for profile %s", profile.Name)
    }

    return nil
}

// extractBaseAddressFromRTSP extracts the base address from the RTSP address.
func extractBaseAddressFromRTSP(rtspAddress string) (string, error) {
    if !strings.HasPrefix(rtspAddress, "rtsp://") {
        return "", errors.New("invalid RTSP address")
    }
    u, err := url.Parse(rtspAddress)
    if err != nil {
        return "", fmt.Errorf("failed to parse RTSP address: %w", err)
    }

    // Extract the hostname (without port)
    baseAddress := u.Hostname()
    if baseAddress == "" {
        return "", errors.New("empty host in RTSP address")
    }

    return baseAddress, nil
}

// getMediaProfiles retrieves the available media profiles from the ONVIF device.
func getMediaProfiles(deviceInstance *onvif.Device) ([]onvifXsd.Profile, error) {
    response, err := deviceInstance.CallMethod(media.GetProfiles{})
    if err != nil {
        return nil, fmt.Errorf("call to GetProfiles failed: %w", err)
    }

    responseBody, err := io.ReadAll(response.Body)
    if err != nil {
        return nil, fmt.Errorf("failed to read GetProfiles response body: %w", err)
    }

    // Define the response structure
    var envelope struct {
        XMLName xml.Name `xml:"Envelope"`
        Body    struct {
            GetProfilesResponse struct {
                Profiles []onvifXsd.Profile `xml:"Profiles"`
            } `xml:"GetProfilesResponse"`
        } `xml:"Body"`
    }

    err = xml.Unmarshal(responseBody, &envelope)
    if err != nil {
        return nil, fmt.Errorf("failed to unmarshal GetProfiles response: %w", err)
    }

    profiles := envelope.Body.GetProfilesResponse.Profiles
    if len(profiles) == 0 {
        return nil, errors.New("no media profiles found")
    }

    return profiles, nil
}

// setVideoEncoderConfiguration updates the resolution in the video encoder configuration for the given profile.
func setVideoEncoderConfiguration(deviceInstance *onvif.Device, profile onvifXsd.Profile, width, height int) error {
    // Get the current video encoder configuration
    getConfig := media.GetVideoEncoderConfiguration{
        ConfigurationToken: profile.VideoEncoderConfiguration.Token,
    }

    getConfigResponse, err := deviceInstance.CallMethod(getConfig)
    if err != nil {
        return fmt.Errorf("call to GetVideoEncoderConfiguration failed: %w", err)
    }

    responseBody, err := io.ReadAll(getConfigResponse.Body)
    if err != nil {
        return fmt.Errorf("failed to read GetVideoEncoderConfiguration response body: %w", err)
    }

    // Define the response structure
    var envelope struct {
        XMLName xml.Name `xml:"Envelope"`
        Body    struct {
            GetVideoEncoderConfigurationResponse struct {
                VideoEncoderConfiguration onvifXsd.VideoEncoderConfiguration `xml:"VideoEncoderConfiguration"`
            } `xml:"GetVideoEncoderConfigurationResponse"`
        } `xml:"Body"`
    }

    err = xml.Unmarshal(responseBody, &envelope)
    if err != nil {
        return fmt.Errorf("failed to unmarshal GetVideoEncoderConfiguration response: %w", err)
    }

    // Modify the resolution
    config := envelope.Body.GetVideoEncoderConfigurationResponse.VideoEncoderConfiguration
    config.Resolution.Width = xsd.Int(width)
    config.Resolution.Height = xsd.Int(height)

    // Set the new configuration
    setConfig := media.SetVideoEncoderConfiguration{
        Configuration:    config,
        ForcePersistence: true,
    }

    setConfigResponse, err := deviceInstance.CallMethod(setConfig)
    if err != nil {
        return fmt.Errorf("call to SetVideoEncoderConfiguration failed: %w", err)
    }

    responseBody, err = io.ReadAll(setConfigResponse.Body)
    if err != nil {
        return fmt.Errorf("failed to read SetVideoEncoderConfiguration response body: %w", err)
    }

    // Optionally, check for errors in the response
    // ...

    return nil
}
