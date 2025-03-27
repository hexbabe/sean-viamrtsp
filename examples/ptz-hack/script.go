package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/use-go/onvif"
	"github.com/use-go/onvif/media"
	"github.com/use-go/onvif/ptz"
	onvifxsd "github.com/use-go/onvif/xsd/onvif"
)

type CameraInfo struct {
	URL      string
	Username string
	Password string
}

// PTZCapabilities stores information about the camera's PTZ capabilities
type PTZCapabilities struct {
	ServiceCapabilities map[string]bool
	Nodes               []string
	Configurations      []string
	Profiles            []string
	CompatibleConfigs   map[string][]string
	Presets             map[string][]string
	AuxCommands         map[string][]string
}

func NewPTZCapabilities() *PTZCapabilities {
	return &PTZCapabilities{
		ServiceCapabilities: make(map[string]bool),
		CompatibleConfigs:   make(map[string][]string),
		Presets:             make(map[string][]string),
		AuxCommands:         make(map[string][]string),
	}
}

// Main discovery function that orchestrates all steps
func DiscoverCameraPTZCapabilities(ctx context.Context, cameraInfo CameraInfo) (*PTZCapabilities, error) {
	capabilities := NewPTZCapabilities()
	dev, err := onvif.NewDevice(onvif.DeviceParams{
		Xaddr:    cameraInfo.URL,
		Username: cameraInfo.Username,
		Password: cameraInfo.Password,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create ONVIF device: %w", err)
	}

	GetServiceCapabilities(dev, capabilities)

	GetNodes(dev, capabilities)

	GetNodeDetails(dev, capabilities)

	GetConfigurations(dev, capabilities)

	GetConfigurationDetails(dev, capabilities)

	GetConfigurationOptions(dev, capabilities)

	GetMediaProfiles(dev, capabilities)

	GetCompatibleConfigurations(dev, capabilities)

	GetPresets(dev, capabilities)

	PrintCapabilitiesSummary(capabilities)

	return capabilities, nil
}

func GetServiceCapabilities(dev *onvif.Device, capabilities *PTZCapabilities) {
	fmt.Println("Step 1: Getting PTZ service capabilities...")
	capabilitiesReq := ptz.GetServiceCapabilities{}
	capabilitiesRes, err := dev.CallMethod(capabilitiesReq)
	if err != nil {
		fmt.Printf("Warning: Failed to get PTZ service capabilities: %v\n", err)
		return
	}

	body, _ := io.ReadAll(capabilitiesRes.Body)
	defer capabilitiesRes.Body.Close()
	fmt.Printf("PTZ Service Capabilities:\n%s\n\n", string(body))
	
	// Parse capabilities if needed - example structure, adjust based on actual response
	var capsResponse struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			GetServiceCapabilitiesResponse struct {
				Capabilities struct {
					EFlip         bool `xml:"EFlip,attr"`
					Reverse       bool `xml:"Reverse,attr"`
					GetCompatible bool `xml:"GetCompatibleConfigurations,attr"`
				} `xml:"Capabilities"`
			} `xml:"GetServiceCapabilitiesResponse"`
		} `xml:"Body"`
	}
	
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&capsResponse); err == nil {
		caps := capsResponse.Body.GetServiceCapabilitiesResponse.Capabilities
		capabilities.ServiceCapabilities["EFlip"] = caps.EFlip
		capabilities.ServiceCapabilities["Reverse"] = caps.Reverse
		capabilities.ServiceCapabilities["GetCompatible"] = caps.GetCompatible
	}
}

func GetNodes(dev *onvif.Device, capabilities *PTZCapabilities) {
	fmt.Println("Step 2: Getting PTZ nodes...")
	nodesReq := ptz.GetNodes{}
	nodesRes, err := dev.CallMethod(nodesReq)
	if err != nil {
		fmt.Printf("Warning: Failed to get PTZ nodes: %v\n", err)
		return
	}

	body, _ := io.ReadAll(nodesRes.Body)
	defer nodesRes.Body.Close()
	fmt.Printf("PTZ Nodes:\n%s\n\n", string(body))
	
	// Parse nodes from XML
	var nodesResponse struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			GetNodesResponse struct {
				PTZNode []struct {
					Token string `xml:"token,attr"`
				} `xml:"PTZNode"`
			} `xml:"GetNodesResponse"`
		} `xml:"Body"`
	}
	
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&nodesResponse); err == nil {
		for _, node := range nodesResponse.Body.GetNodesResponse.PTZNode {
			capabilities.Nodes = append(capabilities.Nodes, node.Token)
		}
	}
}

func GetNodeDetails(dev *onvif.Device, capabilities *PTZCapabilities) {
	fmt.Println("Step 3: Getting detailed information for each PTZ node...")
	for _, nodeToken := range capabilities.Nodes {
		nodeReq := ptz.GetNode{
			NodeToken: onvifxsd.ReferenceToken(nodeToken),
		}
		
		nodeRes, err := dev.CallMethod(nodeReq)
		if err != nil {
			fmt.Printf("Warning: Failed to get details for node %s: %v\n", nodeToken, err)
			continue
		}
		
		body, _ := io.ReadAll(nodeRes.Body)
		defer nodeRes.Body.Close()
		fmt.Printf("Node %s details:\n%s\n\n", nodeToken, string(body))
		
		// Extract auxiliary commands if available
		var nodeResponse struct {
			XMLName xml.Name `xml:"Envelope"`
			Body    struct {
				GetNodeResponse struct {
					PTZNode struct {
						AuxiliaryCommands []string `xml:"AuxiliaryCommands"`
					} `xml:"PTZNode"`
				} `xml:"GetNodeResponse"`
			} `xml:"Body"`
		}
		
		if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&nodeResponse); err == nil {
			capabilities.AuxCommands[nodeToken] = nodeResponse.Body.GetNodeResponse.PTZNode.AuxiliaryCommands
		}
	}
}

func GetConfigurations(dev *onvif.Device, capabilities *PTZCapabilities) {
	fmt.Println("Step 4: Getting PTZ configurations...")
	configsReq := ptz.GetConfigurations{}
	configsRes, err := dev.CallMethod(configsReq)
	if err != nil {
		fmt.Printf("Warning: Failed to get PTZ configurations: %v\n", err)
		return
	}

	body, _ := io.ReadAll(configsRes.Body)
	defer configsRes.Body.Close()
	fmt.Printf("PTZ Configurations:\n%s\n\n", string(body))
	
	// Parse configurations from XML
	var configsResponse struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			GetConfigurationsResponse struct {
				PTZConfiguration []struct {
					Token string `xml:"token,attr"`
				} `xml:"PTZConfiguration"`
			} `xml:"GetConfigurationsResponse"`
		} `xml:"Body"`
	}
	
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&configsResponse); err == nil {
		for _, config := range configsResponse.Body.GetConfigurationsResponse.PTZConfiguration {
			capabilities.Configurations = append(capabilities.Configurations, config.Token)
		}
	}
}

func GetConfigurationDetails(dev *onvif.Device, capabilities *PTZCapabilities) {
	fmt.Println("Step 5: Getting detailed information for each PTZ configuration...")
	for _, configToken := range capabilities.Configurations {
		configReq := ptz.GetConfiguration{
			ProfileToken: onvifxsd.ReferenceToken(configToken),
		}
		
		configRes, err := dev.CallMethod(configReq)
		if err != nil {
			fmt.Printf("Warning: Failed to get details for configuration %s: %v\n", configToken, err)
			continue
		}
		
		body, _ := io.ReadAll(configRes.Body)
		defer configRes.Body.Close()
		fmt.Printf("Configuration %s details:\n%s\n\n", configToken, string(body))
	}
}

func GetConfigurationOptions(dev *onvif.Device, capabilities *PTZCapabilities) {
	fmt.Println("Step 6: Getting configuration options for each PTZ configuration...")
	for _, configToken := range capabilities.Configurations {
		optionsReq := ptz.GetConfigurationOptions{
			ProfileToken: onvifxsd.ReferenceToken(configToken),
		}
		
		optionsRes, err := dev.CallMethod(optionsReq)
		if err != nil {
			fmt.Printf("Warning: Failed to get options for configuration %s: %v\n", configToken, err)
			continue
		}
		
		body, _ := io.ReadAll(optionsRes.Body)
		defer optionsRes.Body.Close()
		fmt.Printf("Configuration %s options:\n%s\n\n", configToken, string(body))
	}
}

func GetMediaProfiles(dev *onvif.Device, capabilities *PTZCapabilities) {
	fmt.Println("Step 7: Getting media profiles...")
	profilesReq := media.GetProfiles{}
	profilesRes, err := dev.CallMethod(profilesReq)
	if err != nil {
		fmt.Printf("Warning: Failed to get media profiles: %v\n", err)
		return
	}

	body, _ := io.ReadAll(profilesRes.Body)
	defer profilesRes.Body.Close()
	fmt.Printf("Media Profiles:\n%s\n\n", string(body))
	
	// Parse profiles from XML
	var profilesResponse struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			GetProfilesResponse struct {
				Profiles []struct {
					Token string `xml:"token,attr"`
				} `xml:"Profiles"`
			} `xml:"GetProfilesResponse"`
		} `xml:"Body"`
	}
	
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&profilesResponse); err == nil {
		for _, profile := range profilesResponse.Body.GetProfilesResponse.Profiles {
			capabilities.Profiles = append(capabilities.Profiles, profile.Token)
		}
	}
}

func GetCompatibleConfigurations(dev *onvif.Device, capabilities *PTZCapabilities) {
	fmt.Println("Step 8: Getting compatible PTZ configurations for each media profile...")
	for _, profileToken := range capabilities.Profiles {
		// Skip if GetCompatible capability is not supported
		if val, ok := capabilities.ServiceCapabilities["GetCompatible"]; ok && !val {
			fmt.Printf("GetCompatibleConfigurations not supported by this device, skipping...\n")
			return
		}
		
		compatReq := ptz.GetCompatibleConfigurations{
			ProfileToken: onvifxsd.ReferenceToken(profileToken),
		}
		
		compatRes, err := dev.CallMethod(compatReq)
		if err != nil {
			fmt.Printf("Warning: Failed to get compatible configurations for profile %s: %v\n", profileToken, err)
			continue
		}
		
		body, _ := io.ReadAll(compatRes.Body)
		defer compatRes.Body.Close()
		fmt.Printf("Compatible configurations for profile %s:\n%s\n\n", profileToken, string(body))
		
		// Parse compatible configurations from XML
		var compatResponse struct {
			XMLName xml.Name `xml:"Envelope"`
			Body    struct {
				GetCompatibleConfigurationsResponse struct {
					PTZConfiguration []struct {
						Token string `xml:"token,attr"`
					} `xml:"PTZConfiguration"`
				} `xml:"GetCompatibleConfigurationsResponse"`
			} `xml:"Body"`
		}
		
		if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&compatResponse); err == nil {
			var compatConfigs []string
			for _, config := range compatResponse.Body.GetCompatibleConfigurationsResponse.PTZConfiguration {
				compatConfigs = append(compatConfigs, config.Token)
			}
			capabilities.CompatibleConfigs[profileToken] = compatConfigs
		}
	}
}

func GetPresets(dev *onvif.Device, capabilities *PTZCapabilities) {
	fmt.Println("Step 10: Getting presets for each profile...")
	for _, profileToken := range capabilities.Profiles {
		presetsReq := ptz.GetPresets{
			ProfileToken: onvifxsd.ReferenceToken(profileToken),
		}
		
		presetsRes, err := dev.CallMethod(presetsReq)
		if err != nil {
			fmt.Printf("Warning: Failed to get presets for profile %s: %v\n", profileToken, err)
			continue
		}
		
		body, _ := io.ReadAll(presetsRes.Body)
		defer presetsRes.Body.Close()
		fmt.Printf("Presets for profile %s:\n%s\n\n", profileToken, string(body))
		
		// Parse presets from XML
		var presetsResponse struct {
			XMLName xml.Name `xml:"Envelope"`
			Body    struct {
				GetPresetsResponse struct {
					Preset []struct {
						Token string `xml:"token,attr"`
						Name  string `xml:"Name"`
					} `xml:"Preset"`
				} `xml:"GetPresetsResponse"`
			} `xml:"Body"`
		}
		
		if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&presetsResponse); err == nil {
			var presets []string
			for _, preset := range presetsResponse.Body.GetPresetsResponse.Preset {
				presets = append(presets, preset.Token)
			}
			capabilities.Presets[profileToken] = presets
		}
	}
}

func PrintCapabilitiesSummary(capabilities *PTZCapabilities) {
	fmt.Println("\n=== Camera PTZ Capabilities Summary ===")
	fmt.Printf("PTZ Nodes: %v\n", capabilities.Nodes)
	fmt.Printf("PTZ Configurations: %v\n", capabilities.Configurations)
	fmt.Printf("Media Profiles: %v\n", capabilities.Profiles)
	
	for profile, configs := range capabilities.CompatibleConfigs {
		fmt.Printf("Profile %s compatible configurations: %v\n", profile, configs)
	}
	
	for profile, presets := range capabilities.Presets {
		fmt.Printf("Profile %s presets: %v\n", profile, presets)
	}
	
	for node, commands := range capabilities.AuxCommands {
		if len(commands) > 0 {
			fmt.Printf("Node %s auxiliary commands: %v\n", node, commands)
		}
	}
}

func main() {
    ctx := context.Background()

    // Check if the required arguments are provided
    if len(os.Args) < 4 {
        log.Fatalf("Usage: %s <url> <username> <password>", os.Args[0])
    }

    // Parse positional arguments
    url := os.Args[1]
    username := os.Args[2]
    password := os.Args[3]

    cameraInfo := CameraInfo{
        URL:      url,
        Username: username,
        Password: password,
    }

    _, err := DiscoverCameraPTZCapabilities(ctx, cameraInfo)
    if err != nil {
        log.Fatalf("Error discovering camera capabilities: %v", err)
    }
}
