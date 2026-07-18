package fskitproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	Version           uint16 = 2
	HeaderSize               = 40
	DefaultMaxPayload        = 16 << 20
	OpenFlagSnapshot  uint32 = 1 << 31
)

var (
	frameMagic      = [4]byte{'C', 'F', 'S', 'P'}
	descriptorMagic = [4]byte{'C', 'F', 'S', 'R'}
)

type Kind uint8

const (
	KindRequest  Kind = 1
	KindResponse Kind = 2
)

type Op uint8

const (
	OpHello Op = iota + 1
	OpPing
	OpGetattr
	OpReadDir
	OpOpen
	OpCreate
	OpRead
	OpWrite
	OpFsync
	OpFlush
	OpRelease
	OpTruncate
	OpMkdir
	OpRename
	OpUnlink
	OpRmdir
	OpStatfs
	OpSync
	OpNamespaceVersion
	OpSetattr
	OpGetXattr
	OpSetXattr
	OpListXattrs
)

const (
	SetAttrMode uint32 = 1 << iota
	SetAttrUID
	SetAttrGID
	SetAttrAccessTime
	SetAttrModifyTime
)

type XattrPolicy uint32

const (
	XattrAlwaysSet XattrPolicy = iota
	XattrMustCreate
	XattrMustReplace
	XattrDelete
)

type EntryType uint8

const (
	EntryUnknown EntryType = iota
	EntryFile
	EntryDirectory
	EntrySymlink
)

type Frame struct {
	Kind       Kind
	Op         Op
	Flags      uint32
	RequestID  uint64
	Generation uint64
	Status     int32
	Payload    []byte
}

func ReadFrame(reader io.Reader, maxPayload uint32) (Frame, error) {
	if maxPayload == 0 {
		maxPayload = DefaultMaxPayload
	}
	header := make([]byte, HeaderSize)
	if _, err := io.ReadFull(reader, header); err != nil {
		return Frame{}, err
	}
	if string(header[:4]) != string(frameMagic[:]) {
		return Frame{}, errors.New("invalid FSKit protocol magic")
	}
	if version := binary.LittleEndian.Uint16(header[4:6]); version != Version {
		return Frame{}, fmt.Errorf("unsupported FSKit protocol version %d", version)
	}
	payloadLength := binary.LittleEndian.Uint32(header[32:36])
	if payloadLength > maxPayload {
		return Frame{}, fmt.Errorf("FSKit protocol payload %d exceeds limit %d", payloadLength, maxPayload)
	}
	payload := make([]byte, payloadLength)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return Frame{}, err
	}
	return Frame{
		Kind:       Kind(header[6]),
		Op:         Op(header[7]),
		Flags:      binary.LittleEndian.Uint32(header[8:12]),
		RequestID:  binary.LittleEndian.Uint64(header[12:20]),
		Generation: binary.LittleEndian.Uint64(header[20:28]),
		Status:     int32(binary.LittleEndian.Uint32(header[28:32])),
		Payload:    payload,
	}, nil
}

func WriteFrame(writer io.Writer, frame Frame, maxPayload uint32) error {
	if maxPayload == 0 {
		maxPayload = DefaultMaxPayload
	}
	if len(frame.Payload) > int(maxPayload) {
		return fmt.Errorf("FSKit protocol payload %d exceeds limit %d", len(frame.Payload), maxPayload)
	}
	header := make([]byte, HeaderSize)
	copy(header[:4], frameMagic[:])
	binary.LittleEndian.PutUint16(header[4:6], Version)
	header[6] = byte(frame.Kind)
	header[7] = byte(frame.Op)
	binary.LittleEndian.PutUint32(header[8:12], frame.Flags)
	binary.LittleEndian.PutUint64(header[12:20], frame.RequestID)
	binary.LittleEndian.PutUint64(header[20:28], frame.Generation)
	binary.LittleEndian.PutUint32(header[28:32], uint32(frame.Status))
	binary.LittleEndian.PutUint32(header[32:36], uint32(len(frame.Payload)))
	if err := writeAll(writer, header); err != nil {
		return err
	}
	return writeAll(writer, frame.Payload)
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
		data = data[n:]
	}
	return nil
}

type Entry struct {
	Path        string
	Name        string
	NodeID      uint64
	ParentID    uint64
	Type        EntryType
	Mode        uint32
	UID         uint32
	GID         uint32
	Size        uint64
	AllocSize   uint64
	ModTime     time.Time
	ChangeTime  time.Time
	AccessTime  time.Time
	NamespaceID uint64
}

type StatFS struct {
	BlockSize      uint32
	IOSize         uint32
	TotalBytes     uint64
	AvailableBytes uint64
	FreeBytes      uint64
	UsedBytes      uint64
	TotalFiles     uint64
	FreeFiles      uint64
}

type Descriptor struct {
	Generation uint64
	SocketPath string
	Token      []byte
}

func EncodeDescriptor(descriptor Descriptor) ([]byte, error) {
	if descriptor.Generation == 0 {
		return nil, errors.New("descriptor generation is required")
	}
	if descriptor.SocketPath == "" || len(descriptor.SocketPath) > 4096 {
		return nil, errors.New("descriptor socket path is invalid")
	}
	if len(descriptor.Token) < 16 || len(descriptor.Token) > 256 {
		return nil, errors.New("descriptor token length is invalid")
	}
	encoder := NewEncoder(24 + len(descriptor.SocketPath) + len(descriptor.Token))
	encoder.Raw(descriptorMagic[:])
	encoder.Uint16(Version)
	encoder.Uint16(0)
	encoder.Uint64(descriptor.Generation)
	encoder.String(descriptor.SocketPath)
	encoder.Bytes(descriptor.Token)
	return encoder.Data(), nil
}

func DecodeDescriptor(data []byte) (Descriptor, error) {
	decoder := NewDecoder(data)
	magic, err := decoder.Raw(4)
	if err != nil || string(magic) != string(descriptorMagic[:]) {
		return Descriptor{}, errors.New("invalid FSKit resource descriptor magic")
	}
	version, err := decoder.Uint16()
	if err != nil {
		return Descriptor{}, err
	}
	if version != Version {
		return Descriptor{}, fmt.Errorf("unsupported FSKit resource descriptor version %d", version)
	}
	if _, err := decoder.Uint16(); err != nil {
		return Descriptor{}, err
	}
	generation, err := decoder.Uint64()
	if err != nil || generation == 0 {
		return Descriptor{}, errors.New("invalid FSKit resource descriptor generation")
	}
	socketPath, err := decoder.String(4096)
	if err != nil || socketPath == "" {
		return Descriptor{}, errors.New("invalid FSKit resource descriptor socket path")
	}
	token, err := decoder.Bytes(256)
	if err != nil || len(token) < 16 {
		return Descriptor{}, errors.New("invalid FSKit resource descriptor token")
	}
	if err := decoder.Done(); err != nil {
		return Descriptor{}, err
	}
	return Descriptor{Generation: generation, SocketPath: socketPath, Token: token}, nil
}
