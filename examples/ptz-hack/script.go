package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/use-go/onvif"
	"github.com/use-go/onvif/device"
	"github.com/use-go/onvif/ptz"
	onvifxsd "github.com/use-go/onvif/xsd/onvif"
)

type CameraInfo struct {
	URL      string
	Username string
	Password string
	Profile  string
}

func GetCameraPTZInfo(ctx context.Context) error {
	cameraInfo := CameraInfo{
		URL:      "10.1.4.255:80",
		Username: "admin",
		Password: "checkmate1",
		Profile:  "001",
	}

	dev, err := onvif.NewDevice(onvif.DeviceParams{
		Xaddr:    cameraInfo.URL,
		Username: cameraInfo.Username,
		Password: cameraInfo.Password,
	})
	if err != nil {
		return fmt.Errorf("failed to create ONVIF device: %w", err)
	}

	res, err := dev.CallMethod(device.GetDeviceInformation{})
	if err != nil {
		return fmt.Errorf("failed to get device information: %w", err)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	res.Body.Close()

	var info struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			GetDeviceInformationResponse struct {
				Manufacturer    string `xml:"Manufacturer"`
				Model           string `xml:"Model"`
				FirmwareVersion string `xml:"FirmwareVersion"`
				SerialNumber    string `xml:"SerialNumber"`
			} `xml:"GetDeviceInformationResponse"`
		} `xml:"Body"`
	}

	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&info); err != nil {
		return fmt.Errorf("failed to decode device information: %w", err)
	}

	fmt.Printf("Device Information:\n")
	fmt.Printf("  Manufacturer: %s\n", info.Body.GetDeviceInformationResponse.Manufacturer)
	fmt.Printf("  Model: %s\n", info.Body.GetDeviceInformationResponse.Model)
	fmt.Printf("  Serial Number: %s\n", info.Body.GetDeviceInformationResponse.SerialNumber)
	fmt.Printf("  Firmware Version: %s\n", info.Body.GetDeviceInformationResponse.FirmwareVersion)

	// Get all available PTZ nodes
	fmt.Println("\nGetting all available PTZ nodes...")
	nodesReq := ptz.GetNodes{}
	res, err = dev.CallMethod(nodesReq)
	if err != nil {
		return fmt.Errorf("failed to get PTZ nodes: %w", err)
	}

	body, err = io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	res.Body.Close()

	var nodesResponse struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			GetNodesResponse struct {
				PTZNode []struct {
					Name  string `xml:"Name"`
					Token string `xml:"token,attr"`
				} `xml:"PTZNode"`
			} `xml:"GetNodesResponse"`
		} `xml:"Body"`
	}

	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&nodesResponse); err != nil {
		return fmt.Errorf("failed to decode PTZ nodes response: %w", err)
	}

	fmt.Printf("\nAvailable PTZ Nodes:\n")
	for _, node := range nodesResponse.Body.GetNodesResponse.PTZNode {
		fmt.Printf("  Name: %s, Token: %s\n", node.Name, node.Token)
	}

	req := ptz.GetNode{
		NodeToken: onvifxsd.ReferenceToken(cameraInfo.Profile),
	}

	res, err = dev.CallMethod(req)
	if err != nil {
		return fmt.Errorf("failed to get PTZ node: %w", err)
	}

	body, err = io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	res.Body.Close()

	var nodeResponse struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			GetNodeResponse struct {
				PTZNode struct {
					Name              string `xml:"Name"`
					Token             string `xml:"token,attr"`
					SupportedPTZSpaces struct {
						AbsolutePanTiltPositionSpace struct {
							URI string `xml:"URI"`
						} `xml:"AbsolutePanTiltPositionSpace"`
						AbsoluteZoomPositionSpace struct {
							URI string `xml:"URI"`
						} `xml:"AbsoluteZoomPositionSpace"`
						RelativePanTiltTranslationSpace struct {
							URI string `xml:"URI"`
						} `xml:"RelativePanTiltTranslationSpace"`
						RelativeZoomTranslationSpace struct {
							URI string `xml:"URI"`
						} `xml:"RelativeZoomTranslationSpace"`
						ContinuousPanTiltVelocitySpace struct {
							URI string `xml:"URI"`
						} `xml:"ContinuousPanTiltVelocitySpace"`
						ContinuousZoomVelocitySpace struct {
							URI string `xml:"URI"`
						} `xml:"ContinuousZoomVelocitySpace"`
					} `xml:"SupportedPTZSpaces"`
				} `xml:"PTZNode"`
			} `xml:"GetNodeResponse"`
		} `xml:"Body"`
	}

	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&nodeResponse); err != nil {
		return fmt.Errorf("failed to decode PTZ node response: %w", err)
	}

	fmt.Printf("\nFull PTZ Node Response:\n")
	fmt.Printf("%+v\n", nodeResponse)
	fmt.Println("\nDemonstrating PTZ Control (will move camera right for 2 seconds)...")

	moveReq := ptz.ContinuousMove{
		ProfileToken: onvifxsd.ReferenceToken(cameraInfo.Profile),
		Velocity: onvifxsd.PTZSpeed{
			PanTilt: onvifxsd.Vector2D{
				X: 0.2,
				Y: 0,
			},
			Zoom: onvifxsd.Vector1D{
				X: 0,
			},
		},
	}

	_, err = dev.CallMethod(moveReq)
	if err != nil {
		return fmt.Errorf("failed to execute continuous move: %w", err)
	}

	time.Sleep(2 * time.Second)

	stopReq := ptz.Stop{
		ProfileToken: onvifxsd.ReferenceToken(cameraInfo.Profile),
		PanTilt:      true,
		Zoom:         true,
	}

	_, err = dev.CallMethod(stopReq)
	if err != nil {
		return fmt.Errorf("failed to stop movement: %w", err)
	}

	fmt.Println("PTZ movement completed.")
	return nil
}

func main() {
	ctx := context.Background()
	if err := GetCameraPTZInfo(ctx); err != nil {
		log.Fatalf("Error: %v", err)
	}
}
