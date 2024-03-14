package silk

import "C"
import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"syscall"
	"unsafe"

	"github.com/0xrawsec/golang-utils/log"
	"golang.org/x/sys/windows"
)

const (
	STX                 byte = 2           // 文件开头如果是 0x02 解码时需要丢弃
	Header                   = "#!SILK_V3" // 文件头
	HeaderLen                = len(Header) // 文件头长度 = 9
	MAX_BYTES_PER_FRAME      = 250         // Equals peak bitrate of 100 kbps
	MAX_INPUT_FRAMES         = 5
	FRAME_LENGTH_MS          = 20
	MAX_API_FS_KHZ           = 48
	// 默认值
	defaultSampleRate = 24000
)

func checkHeader(reader *bufio.Reader) error {
	first, err := reader.Peek(1)
	if err != nil {
		log.Warn("io error / failed to peek first byte: %+v", err)
		return fmt.Errorf("failed to peek first byte: %w", err)
	}
	// 如果第一位是 0x02 需要丢弃
	// 安卓移植版说明:
	// https://wufengxue.github.io/2019/04/17/wechat-voice-codec-amr.html
	// kn007 兼容版本:
	// https://github.com/kn007/silk-v3-decoder/blob/master/silk/test/Decoder.c#L187
	// 原始开源版本:(不识别 0x02 开头的文件)
	// https://github.com/gaozehua/SILKCodec/blob/master/SILK_SDK_SRC_ARM/test/Decoder.c#L182
	if first[0] == STX {
		log.Info("first byte is STX(%x), read it", STX)
		stx, err := reader.ReadByte()
		if err != nil {
			log.Warn("read first byte error: %+v", err)
			return fmt.Errorf("failed to read first byte: %w", err)
		}
		if stx != STX {
			log.Warn("read first byte not STX: %x", stx)
			return fmt.Errorf("invalid first byte: %d, expected=%d", stx, STX)
		}
	}
	// 文件头
	var header = make([]byte, HeaderLen)
	n, err := io.ReadFull(reader, header)
	if err != nil {
		log.Warn("failed to read file header: %+v", err)
		return fmt.Errorf("failed to read file header: %w", err)
	}
	if n != HeaderLen {
		log.Warn("invalid file header, read %d bytes, expected %d", n, HeaderLen)
		return fmt.Errorf("invalid file header, length=%d, expected=%d", n, HeaderLen)
	}
	if string(header) != Header {
		log.Warn("invalid file header %q expected %q", header, HeaderLen)
		return fmt.Errorf("invalid file header, got=%q, expected=%q", header, Header)
	}
	return nil
}

func NewSilkDecoder() *silk {
	s := new(silk)
	s.init()
	return s
}

type silk struct {
	dll *syscall.DLL
}

func (s *silk) init() error {
	silkDll, err := syscall.LoadDLL(`dllsilk.dll`)
	if err != nil {
		return err
	}
	s.dll = silkDll
	return nil
}

func (s silk) Decode(src io.Reader) ([]byte, error) {
	var reader = bufio.NewReader(src)
	/* Check Silk header */
	if err := checkHeader(reader); err != nil {
		return nil, err
	}
	var blockIndex int
	out := &bytes.Buffer{}
	handle, err := s.createDecoder()
	if err != nil {
		return nil, err
	}
	err = s.setSampleRate(handle, 16000)
	if err != nil {
		return nil, err
	}
	err = s.setFramesPerPacket(handle, 1)
	if err != nil {
		return nil, err
	}
	// in 对应 C 源码中 payload(SKP_uint8 数组), buf 对应 out(SKP_int16 数组)
	var in = make([]byte, 1024) // Decoder.c 中 MAX_BYTES_PER_FRAME 和 Encoder.c 不一样哦
	// 20ms FRAME_LENGTH_MS=20 MAX_API_FS_KHZ=48
	var frameSize = (FRAME_LENGTH_MS * MAX_API_FS_KHZ) << 1
	// frameSize 个 SKP_int16，这里是 []byte 所以 *2
	var buf = make([]byte, frameSize*2) // 相当于 [frameSize]int16 大小
	for {
		blockIndex++
		var nByte int16 // 先读取 block 大小, 占两个字节，用 int16 接收
		err = binary.Read(reader, binary.LittleEndian, &nByte)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
				break
			}
			return nil, fmt.Errorf("failed to read block size: %w", err)
		}
		if nByte < 0 {
			break // 是 footer 部分, 没有 block 内容
		}
		if int(nByte) > len(in) { // 兜底 or 报错?
			in = make([]byte, nByte)
		}
		// 再读取 block 内容，长度就是 nByte
		n, err := io.ReadFull(reader, in[:nByte])
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
				break
			}
			return nil, fmt.Errorf("failed to read block: %w", err)
		}
		if n != int(nByte) {
			return nil, fmt.Errorf("invalid block")
		}
		length, err := s.decode(handle, in[:n], n, buf, nByte)
		if err != nil {
			return nil, err
		}
		_, err = out.Write(buf[:length])
		if err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

func (s silk) createDecoder() (uintptr, error) {
	f, err := s.dll.FindProc("CreateDecoder")
	if err != nil {
		return 0, err
	}
	handle, _, err := f.Call()
	if err != nil && !errors.Is(err, windows.SEVERITY_SUCCESS) {
		return 0, err
	}
	return handle, nil
}

func (s silk) closeDecoder(handle uintptr) error {
	f, err := s.dll.FindProc("CloseDecoder")
	if err != nil {
		return err
	}
	_, _, err = f.Call(handle)
	if err != nil && !errors.Is(err, windows.SEVERITY_SUCCESS) {
		return err
	}
	return nil
}

func (s silk) setSampleRate(handle uintptr, sample int) error {
	f, err := s.dll.FindProc("setSampleRate")
	if err != nil {
		return err
	}
	_, _, err = f.Call(handle, uintptr(sample))
	if err != nil && !errors.Is(err, windows.SEVERITY_SUCCESS) {
		return err
	}
	return nil
}

func (s silk) setFramesPerPacket(handle uintptr, perPacket int) error {
	f, err := s.dll.FindProc("setFramesPerPacket")
	if err != nil {
		return err
	}
	_, _, err = f.Call(handle, uintptr(perPacket))
	if err != nil && !errors.Is(err, windows.SEVERITY_SUCCESS) {
		return err
	}
	return nil
}

func (s silk) decode(handle uintptr, inData []byte, inDataLength int, outData []byte, outDataLength int16) (int, error) {
	f, err := s.dll.FindProc("Decode")
	if err != nil {
		return 0, err
	}
	_, _, err = f.Call(handle, uintptr(unsafe.Pointer(&inData[0])), uintptr(inDataLength), uintptr(unsafe.Pointer(&outData[0])), uintptr(unsafe.Pointer(&outDataLength)))
	if err != nil && !errors.Is(err, windows.SEVERITY_SUCCESS) {
		return 0, err
	}
	return int(outDataLength * 2), nil
}

// dst:二进制pcm数据
// saplerate：采样率 8000/16000
//
//numchannel:1=单声道，2=多声道
func pcmToWav(dst []byte, numchannel int, saplerate int) (resDst []byte) {
	byteDst := dst
	longSampleRate := saplerate
	byteRate := 16 * saplerate * numchannel / 8
	totalAudioLen := len(byteDst)
	totalDataLen := totalAudioLen + 36
	var header = make([]byte, 44)
	// RIFF/WAVE header
	header[0] = 'R'
	header[1] = 'I'
	header[2] = 'F'
	header[3] = 'F'
	header[4] = byte(totalDataLen & 0xff)
	header[5] = byte((totalDataLen >> 8) & 0xff)
	header[6] = byte((totalDataLen >> 16) & 0xff)
	header[7] = byte((totalDataLen >> 24) & 0xff)
	//WAVE
	header[8] = 'W'
	header[9] = 'A'
	header[10] = 'V'
	header[11] = 'E'
	// 'fmt ' chunk
	header[12] = 'f'
	header[13] = 'm'
	header[14] = 't'
	header[15] = ' '
	// 4 bytes: size of 'fmt ' chunk
	header[16] = 16
	header[17] = 0
	header[18] = 0
	header[19] = 0
	// format = 1
	header[20] = 1
	header[21] = 0
	header[22] = byte(numchannel)
	header[23] = 0
	header[24] = byte(longSampleRate & 0xff)
	header[25] = byte((longSampleRate >> 8) & 0xff)
	header[26] = byte((longSampleRate >> 16) & 0xff)
	header[27] = byte((longSampleRate >> 24) & 0xff)
	header[28] = byte(byteRate & 0xff)
	header[29] = byte((byteRate >> 8) & 0xff)
	header[30] = byte((byteRate >> 16) & 0xff)
	header[31] = byte((byteRate >> 24) & 0xff)
	// block align
	header[32] = byte(2 * 16 / 8)
	header[33] = 0
	// bits per sample
	header[34] = 16
	header[35] = 0
	//data
	header[36] = 'd'
	header[37] = 'a'
	header[38] = 't'
	header[39] = 'a'
	header[40] = byte(totalAudioLen & 0xff)
	header[41] = byte((totalAudioLen >> 8) & 0xff)
	header[42] = byte((totalAudioLen >> 16) & 0xff)
	header[43] = byte((totalAudioLen >> 24) & 0xff)

	headerDst := header
	resDst = append(headerDst, dst...)
	return
}

func SilkToWav(src io.Reader) (io.Reader, error) {
	decoder := NewSilkDecoder()
	data, err := decoder.Decode(src)
	if err != nil {
		return nil, err
	}
	rData := pcmToWav(data, 2, 16000)
	return bytes.NewReader(rData), nil
}
