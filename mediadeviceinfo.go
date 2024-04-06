package mediadevices

import "github.com/ABConnectIO/mediadevices/pkg/driver"

// MediaDeviceType enumerates type of media device.
type MediaDeviceType int

// MediaDeviceType definitions.
const (
	VideoInput MediaDeviceType = iota + 1
	AudioInput
	AudioOutput
)

func (m MediaDeviceType) String() string {
	names := [...]string{
		"Video Input",
		"Audio Input",
		"Audio Output",
	}

	// Check if the enum value is within bounds of the array
	if m < VideoInput || m > AudioOutput {
		return "Unknown"
	}

	// Return the string representation of the enum value
	return names[m-1]
}

// MediaDeviceInfo represents https://w3c.github.io/mediacapture-main/#dom-mediadeviceinfo
type MediaDeviceInfo struct {
	DeviceID   string
	Kind       MediaDeviceType
	Label      string
	DeviceType driver.DeviceType
	Name       string
}
