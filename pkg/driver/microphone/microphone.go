package microphone

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/ABConnectIO/mediadevices/internal/logging"
	"github.com/ABConnectIO/mediadevices/pkg/driver"
	"github.com/ABConnectIO/mediadevices/pkg/io/audio"
	"github.com/ABConnectIO/mediadevices/pkg/prop"
	"github.com/ABConnectIO/mediadevices/pkg/wave"
	"github.com/gen2brain/malgo"
)

const (
	maxDeviceIDLength = 20
	// TODO: should replace this with a more flexible approach
	sampleRateStep    = 1000
	initialBufferSize = 1024
)

var logger = logging.NewLogger("mediadevices/driver/microphone")
var ctx *malgo.AllocatedContext
var hostEndian binary.ByteOrder
var (
	errUnsupportedFormat = errors.New("the provided audio format is not supported")
)

// var Channel int
var UMC1820Name string = "UMC1820 Multichannel"

type microphone struct {
	malgo.DeviceInfo
	chunkChan       chan []byte
	deviceCloseFunc func()
	// The channel number of interest, if relevant. 0 otherwise. This is used to filter the audio samples for devices that contain multiple microphones.
	channelOfInterest int
}

func init() {
	Initialize()
}

// Initialize finds and registers active playback or capture devices. This is part of an experimental API.
func Initialize() {
	var err error
	ctx, err = malgo.InitContext(nil, malgo.ContextConfig{}, func(message string) {
		logger.Debugf("%v\n", message)
	})
	if err != nil {
		panic(err)
	}

	devices, err := ctx.Devices(malgo.Capture)
	if err != nil {
		panic(err)
	}

	for _, device := range devices {
		info, err := ctx.DeviceInfo(malgo.Capture, device.ID, malgo.Shared)
		if info.Name() == UMC1820Name {
			// Create 10 devices for each input channel on the UMC1820
			for i := 1; i < int(info.Formats[0].Channels); i++ {
				if err == nil {
					priority := driver.PriorityNormal
					if info.IsDefault > 0 {
						priority = driver.PriorityHigh
					}
					driver.GetManager().Register(newMicrophoneForChannel(info, i), driver.Info{
						Label:      fmt.Sprintf("UMC1820-channel%d", i),
						DeviceType: driver.Microphone,
						Priority:   priority,
						Name:       fmt.Sprintf("UMC1820-channel%d", i),
					})
				}
			}
		} else {
			if err == nil {
				priority := driver.PriorityNormal
				if info.IsDefault > 0 {
					priority = driver.PriorityHigh
				}
				driver.GetManager().Register(newMicrophone(info), driver.Info{
					Label:      device.ID.String(),
					DeviceType: driver.Microphone,
					Priority:   priority,
					Name:       info.Name(),
				})
			}
		}
	}

	// Decide which endian
	switch v := *(*uint16)(unsafe.Pointer(&([]byte{0x12, 0x34}[0]))); v {
	case 0x1234:
		hostEndian = binary.BigEndian
	case 0x3412:
		hostEndian = binary.LittleEndian
	default:
		panic(fmt.Sprintf("failed to determine host endianness: %x", v))
	}
}

func newMicrophoneForChannel(info malgo.DeviceInfo, channel int) *microphone {
	if strings.Contains(info.Name(), "UMC") {
		info.Formats[0].Format = malgo.FormatF32
	}

	return &microphone{
		DeviceInfo:        info,
		channelOfInterest: channel,
	}
}

func newMicrophone(info malgo.DeviceInfo) *microphone {
	return newMicrophoneForChannel(info, 0)
}

func (m *microphone) Open() error {
	m.chunkChan = make(chan []byte, 1)
	return nil
}

func (m *microphone) Close() error {
	if m.deviceCloseFunc != nil {
		m.deviceCloseFunc()
	}
	return nil
}

func (m *microphone) AudioRecord(inputProp prop.Media) (audio.Reader, error) {
	var config malgo.DeviceConfig
	var callbacks malgo.DeviceCallbacks

	decoder, err := wave.NewDecoder(&wave.RawFormat{
		SampleSize:  inputProp.SampleSize,
		IsFloat:     inputProp.IsFloat,
		Interleaved: inputProp.IsInterleaved,
	})
	if err != nil {
		return nil, err
	}

	config.DeviceType = malgo.Capture
	config.PerformanceProfile = malgo.LowLatency
	config.SampleRate = uint32(inputProp.SampleRate)
	config.PeriodSizeInMilliseconds = uint32(inputProp.Latency.Milliseconds())
	//FIX: Turn on the microphone with the current device id
	config.Capture.DeviceID = m.ID.Pointer()
	if inputProp.SampleSize == 4 && inputProp.IsFloat {
		config.Capture.Format = malgo.FormatF32
	} else if inputProp.SampleSize == 2 && !inputProp.IsFloat {
		config.Capture.Format = malgo.FormatS16
	} else {
		return nil, errUnsupportedFormat
	}

	// Size of sample for one channel for input data format.
	sizeInBytes := uint32(malgo.SampleSizeInBytes(config.Capture.Format))
	// channelOfInterest := Channel
	// Update to the true number of channel only for the instances which use a specific channel number
	if m.channelOfInterest > 0 {
		config.Capture.Channels = m.Formats[0].Channels
	} else {
		config.Capture.Channels = uint32(inputProp.ChannelCount)
	}

	// Frame sample size for all channels.
	samplesPerFrame := config.Capture.Channels * sizeInBytes

	cancelCtx, cancel := context.WithCancel(context.Background())
	onRecvChunk := func(_, chunk []byte, framecount uint32) {
		var subSample []byte
		if m.channelOfInterest > 0 {
			for i := 0; i < int(framecount); i += 1 {
				i_from := (m.channelOfInterest-1)*int(sizeInBytes) + i*int(samplesPerFrame)
				i_to := i_from + int(sizeInBytes)
				subSample = append(subSample, chunk[i_from:i_to]...)
			}
		} else {
			subSample = chunk
		}

		select {
		case <-cancelCtx.Done():
		case m.chunkChan <- subSample:
		}
	}
	callbacks.Data = onRecvChunk

	device, err := malgo.InitDevice(ctx.Context, config, callbacks)
	if err != nil {
		cancel()
		return nil, err
	}

	err = device.Start()
	if err != nil {
		cancel()
		return nil, err
	}

	var closeDeviceOnce sync.Once
	m.deviceCloseFunc = func() {
		closeDeviceOnce.Do(func() {
			cancel() // Unblock onRecvChunk
			device.Uninit()

			if m.chunkChan != nil {
				close(m.chunkChan)
				m.chunkChan = nil
			}
		})
	}

	var reader audio.Reader = audio.ReaderFunc(func() (wave.Audio, func(), error) {
		chunk, ok := <-m.chunkChan
		if !ok {
			m.deviceCloseFunc()
			return nil, func() {}, io.EOF
		}

		decodedChunk, err := decoder.Decode(hostEndian, chunk, inputProp.ChannelCount)
		// FIXME: the decoder should also fill this information
		switch decodedChunk := decodedChunk.(type) {
		case *wave.Float32Interleaved:
			decodedChunk.Size.SamplingRate = inputProp.SampleRate
		case *wave.Int16Interleaved:
			decodedChunk.Size.SamplingRate = inputProp.SampleRate
		default:
			panic("unsupported format")
		}
		return decodedChunk, func() {}, err
	})

	return reader, nil
}

func (m *microphone) Properties() []prop.Media {
	var supportedProps []prop.Media
	logger.Debug("Querying properties")

	var isBigEndian bool
	// miniaudio only uses the host endian
	if hostEndian == binary.BigEndian {
		isBigEndian = true
	}

	for _, format := range m.Formats {
		// FIXME: Currently support 48kHz only. We need to implement a resampler first.
		// for sampleRate := m.MinSampleRate; sampleRate <= m.MaxSampleRate; sampleRate += sampleRateStep {
		sampleRate := 48000
		var channelCount int = 1
		if format.Channels == 2 {
			channelCount = 2

		}
		supportedProp := prop.Media{
			Audio: prop.Audio{
				ChannelCount: channelCount, //int(format.Channels),
				SampleRate:   int(sampleRate),
				IsBigEndian:  isBigEndian,
				// miniaudio only supports interleaved at the moment
				IsInterleaved: true,
				// FIXME: should change this to a less discrete value
				Latency: time.Millisecond * 20,
			},
		}

		switch malgo.FormatType(format.Format) {
		case malgo.FormatF32:
			supportedProp.SampleSize = 4
			supportedProp.IsFloat = true
		case malgo.FormatS16:
			supportedProp.SampleSize = 2
			supportedProp.IsFloat = false
		}

		supportedProps = append(supportedProps, supportedProp)
		// }
	}
	return supportedProps
}
