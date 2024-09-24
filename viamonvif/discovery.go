package viamonvif

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/use-go/onvif"
	"github.com/use-go/onvif/media"
	onvifXsd "github.com/use-go/onvif/xsd/onvif"
	"go.viam.com/rdk/logging"
)

const (
    // WS-Discovery
    maxUDPMessageBytesSize     = 1536
    standardWSDiscoveryAddress = "239.255.255.250:3702"
    discoveryTimeout           = 10 * time.Second

    // ONVIF
    StreamTypeRTPUnicast   = "RTP-Unicast"
    TransportProtocolRTSP  = "RTSP"
)

// RTSPDiscovery is responsible for discovering RTSP camera devices using WS-Discovery and ONVIF.
type RTSPDiscovery struct {
    multicastAddress string
    logger           logging.Logger
    conn             *net.UDPConn
}

// GetProfilesResponse is the schema the GetProfiles response is formatted in.
type GetProfilesResponse struct {
    XMLName xml.Name `xml:"Envelope"`
    Body    struct {
        GetProfilesResponse struct {
            Profiles []onvifXsd.Profile `xml:"Profiles"`
        } `xml:"GetProfilesResponse"`
    } `xml:"Body"`
}

// GetStreamUriResponse is the schema the GetStreamUri response is formatted in.
type GetStreamUriResponse struct {
    XMLName xml.Name `xml:"Envelope"`
    Body    struct {
        GetStreamUriResponse struct {
            MediaUri onvifXsd.MediaUri `xml:"MediaUri"`
        } `xml:"GetStreamUriResponse"`
    } `xml:"Body"`
}

// NewRTSPDiscovery creates a new RTSPDiscovery instance with default values.
func NewRTSPDiscovery(logger logging.Logger) *RTSPDiscovery {
    return &RTSPDiscovery{
        multicastAddress: standardWSDiscoveryAddress,
        logger:           logger,
    }
}

// close closes the UDP connection if it exists.
func (d *RTSPDiscovery) close() error {
    if d.conn != nil {
        err := d.conn.Close()
        d.conn = nil
        return err
    }
    return nil
}

// generateDiscoveryMessage formats an XML discovery message properly.
func (d *RTSPDiscovery) generateDiscoveryMessage() string {
    messageID := uuid.New().String()
    return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
    <SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope" 
                   xmlns:wsa="http://schemas.xmlsoap.org/ws/2004/08/addressing" 
                   xmlns:wsdd="http://schemas.xmlsoap.org/ws/2005/04/discovery">
        <SOAP-ENV:Header>
            <wsa:MessageID>uuid:%s</wsa:MessageID>
            <wsa:To>urn:schemas-xmlsoap-org:ws:2005:04/discovery</wsa:To>
            <wsa:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</wsa:Action>
        </SOAP-ENV:Header>
        <SOAP-ENV:Body>
            <wsdd:Probe>
                <wsdd:Types>dn:NetworkVideoTransmitter</wsdd:Types>
            </wsdd:Probe>
        </SOAP-ENV:Body>
    </SOAP-ENV:Envelope>`, messageID)
}

// DiscoverRTSPAddresses performs WS-Discovery and extracts the XAddrs field,
// then uses ONVIF queries to get available RTSP addresses.
func (d *RTSPDiscovery) DiscoverRTSPAddresses() ([]string, error) {
    var discoveredRTSPAddresses []string

    addr, err := net.ResolveUDPAddr("udp4", d.multicastAddress)
    if err != nil {
        return nil, fmt.Errorf("failed to resolve UDP address: %w", err)
    }

    d.conn, err = net.ListenUDP("udp4", nil)
    if err != nil {
        return nil, fmt.Errorf("failed to create UDP socket: %w", err)
    }
    defer d.close()

    _, err = d.conn.WriteToUDP([]byte(d.generateDiscoveryMessage()), addr)
    if err != nil {
        return nil, fmt.Errorf("failed to send discovery message: %w", err)
    }

    if err = d.conn.SetReadDeadline(time.Now().Add(discoveryTimeout)); err != nil {
        return nil, fmt.Errorf("failed to set read deadline: %w", err)
    }

    buffer := make([]byte, maxUDPMessageBytesSize)
    for {
        n, _, err := d.conn.ReadFromUDP(buffer)
        if err != nil {
            var netErr net.Error
            if errors.As(err, &netErr) && netErr.Timeout() {
                d.logger.Debug("Timed out after waiting.")
                return discoveredRTSPAddresses, nil
            }
            return nil, fmt.Errorf("error reading from UDP: %w", err)
        }

        response := buffer[:n]
        xaddrs := d.extractXAddrsFromProbeMatch(response)
        d.logger.Infof("Discovered XAddrs: %v", xaddrs)

        // Convert XAddrs to RTSP addresses using ONVIF media service
        for _, xaddr := range xaddrs {
            rtspURLs, err := d.getRTSPStreamURLs(xaddr)
            if err != nil {
                d.logger.Warnf("Failed to get RTSP URLs from %s: %v\n", xaddr, err)
                continue
            }
            discoveredRTSPAddresses = append(discoveredRTSPAddresses, rtspURLs...)
        }
    }
}

// extractXAddrsFromProbeMatch extracts XAddrs from the WS-Discovery ProbeMatch response.
func (d *RTSPDiscovery) extractXAddrsFromProbeMatch(response []byte) []string {
    type ProbeMatch struct {
        XMLName xml.Name `xml:"Envelope"`
        Body    struct {
            ProbeMatches struct {
                ProbeMatch []struct {
                    XAddrs string `xml:"XAddrs"`
                } `xml:"ProbeMatch"`
            } `xml:"ProbeMatches"`
        } `xml:"Body"`
    }

    var probeMatch ProbeMatch
    err := xml.NewDecoder(bytes.NewReader(response)).Decode(&probeMatch)
    if err != nil {
        d.logger.Warnf("error unmarshalling ONVIF discovery xml response: %w\nFull xml resp: %s", err, response)
    }

    var xaddrs []string
    for _, match := range probeMatch.Body.ProbeMatches.ProbeMatch {
        for _, xaddr := range strings.Split(match.XAddrs, " ") {
            parsedURL, err := url.Parse(xaddr)
            if err != nil {
                d.logger.Warnf("failed to parse XAddr %s: %v", xaddr, err)
                continue
            }

            // Ensure only base address (hostname or IP) is used
            baseAddress := parsedURL.Host
            if baseAddress == "" {
                continue
            }

            xaddrs = append(xaddrs, baseAddress)
        }
    }

    return xaddrs
}

// getRTSPStreamURLs uses the ONVIF Media service to get the RTSP stream URLs for all available profiles.
func (d *RTSPDiscovery) getRTSPStreamURLs(deviceServiceURL string) ([]string, error) {
    d.logger.Infof("Connecting to ONVIF device with URL: %s", deviceServiceURL)

    deviceInstance, err := onvif.NewDevice(onvif.DeviceParams{
        Xaddr:    deviceServiceURL,
        Username: "admin", // replace
        Password: "admin", // replace
    })
    if err != nil {
        return nil, fmt.Errorf("failed to connect to ONVIF device: %v", err)
    }

    // Step 1: Call the ONVIF Media service to get the available media profiles
    getProfiles := media.GetProfiles{}
    profilesResponse, err := deviceInstance.CallMethod(getProfiles)
    if err != nil {
        return nil, fmt.Errorf("failed to get media profiles: %v", err)
    }

    profilesBody, err := io.ReadAll(profilesResponse.Body)
    if err != nil {
        return nil, fmt.Errorf("failed to read profiles response body: %v", err)
    }
    d.logger.Infof("GetProfiles response body: %s", profilesBody)
    // Reset the response body reader after logging
    profilesResponse.Body = io.NopCloser(bytes.NewReader(profilesBody))

    var envelope GetProfilesResponse
    err = xml.NewDecoder(profilesResponse.Body).Decode(&envelope)
    if err != nil {
        return nil, fmt.Errorf("failed to decode media profiles response: %v", err)
    }

    if len(envelope.Body.GetProfilesResponse.Profiles) == 0 {
        d.logger.Warn("No media profiles found in the response")
        return nil, fmt.Errorf("no media profiles found")
    }

    d.logger.Infof("Found %d media profiles", len(envelope.Body.GetProfilesResponse.Profiles))
    for i, profile := range envelope.Body.GetProfilesResponse.Profiles {
        d.logger.Infof("Profile %d: Token=%s, Name=%s", i, profile.Token, profile.Name)
    }

    // Step 2: Iterate over all profiles and get the RTSP stream URI for each one
    var rtspUris []string
    for _, profile := range envelope.Body.GetProfilesResponse.Profiles {
        d.logger.Infof("Using profile token: %s", profile.Token)
        rtspProfileToken := onvifXsd.ReferenceToken(profile.Token)

        getStreamUri := media.GetStreamUri{
            StreamSetup: onvifXsd.StreamSetup{
                Stream: onvifXsd.StreamType(StreamTypeRTPUnicast),
                Transport: onvifXsd.Transport{
                    Protocol: onvifXsd.TransportProtocol(TransportProtocolRTSP),
                },
            },
            ProfileToken: rtspProfileToken,
        }

        streamUriResponse, err := deviceInstance.CallMethod(getStreamUri)
        if err != nil {
            d.logger.Warnf("Failed to get RTSP URL for profile %s: %v", profile.Token, err)
            continue
        }

        streamUriBody, err := io.ReadAll(streamUriResponse.Body)
        if err != nil {
            d.logger.Warnf("Failed to read stream URI response body for profile %s: %v", profile.Token, err)
            continue
        }
        d.logger.Infof("GetStreamUri response body for profile %s: %s", profile.Token, streamUriBody)
        // Reset the response body reader after logging
        streamUriResponse.Body = io.NopCloser(bytes.NewReader(streamUriBody))

        var streamUri GetStreamUriResponse
        err = xml.NewDecoder(streamUriResponse.Body).Decode(&streamUri)
        if err != nil {
            d.logger.Warnf("Failed to decode stream URI response for profile %s: %v", profile.Token, err)
            continue
        }

        rtspUris = append(rtspUris, string(streamUri.Body.GetStreamUriResponse.MediaUri.Uri))
    }

    return rtspUris, nil
}
