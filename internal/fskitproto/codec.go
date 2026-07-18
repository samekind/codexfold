package fskitproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

type Encoder struct {
	data []byte
}

func NewEncoder(capacity int) *Encoder {
	return &Encoder{data: make([]byte, 0, capacity)}
}

func (e *Encoder) Data() []byte { return e.data }

func (e *Encoder) Raw(value []byte) { e.data = append(e.data, value...) }

func (e *Encoder) Uint8(value uint8) { e.data = append(e.data, value) }

func (e *Encoder) Uint16(value uint16) {
	start := len(e.data)
	e.data = append(e.data, 0, 0)
	binary.LittleEndian.PutUint16(e.data[start:], value)
}

func (e *Encoder) Uint32(value uint32) {
	start := len(e.data)
	e.data = append(e.data, 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(e.data[start:], value)
}

func (e *Encoder) Uint64(value uint64) {
	start := len(e.data)
	e.data = append(e.data, make([]byte, 8)...)
	binary.LittleEndian.PutUint64(e.data[start:], value)
}

func (e *Encoder) Int64(value int64) { e.Uint64(uint64(value)) }

func (e *Encoder) String(value string) {
	e.Bytes([]byte(value))
}

func (e *Encoder) Bytes(value []byte) {
	e.Uint32(uint32(len(value)))
	e.Raw(value)
}

func (e *Encoder) Time(value time.Time) {
	if value.IsZero() {
		e.Int64(0)
		e.Uint32(0)
		return
	}
	e.Int64(value.Unix())
	e.Uint32(uint32(value.Nanosecond()))
}

func (e *Encoder) Entry(entry Entry) {
	e.String(entry.Path)
	e.String(entry.Name)
	e.Uint64(entry.NodeID)
	e.Uint64(entry.ParentID)
	e.Uint8(uint8(entry.Type))
	e.Uint32(entry.Mode)
	e.Uint32(entry.UID)
	e.Uint32(entry.GID)
	e.Uint64(entry.Size)
	e.Uint64(entry.AllocSize)
	e.Time(entry.ModTime)
	e.Time(entry.ChangeTime)
	e.Time(entry.AccessTime)
	e.Uint64(entry.NamespaceID)
}

func (e *Encoder) StatFS(stat StatFS) {
	e.Uint32(stat.BlockSize)
	e.Uint32(stat.IOSize)
	e.Uint64(stat.TotalBytes)
	e.Uint64(stat.AvailableBytes)
	e.Uint64(stat.FreeBytes)
	e.Uint64(stat.UsedBytes)
	e.Uint64(stat.TotalFiles)
	e.Uint64(stat.FreeFiles)
}

type Decoder struct {
	data   []byte
	offset int
}

func NewDecoder(data []byte) *Decoder { return &Decoder{data: data} }

func (d *Decoder) remaining() int { return len(d.data) - d.offset }

func (d *Decoder) Raw(length int) ([]byte, error) {
	if length < 0 || d.remaining() < length {
		return nil, errors.New("truncated FSKit protocol payload")
	}
	value := d.data[d.offset : d.offset+length]
	d.offset += length
	return value, nil
}

func (d *Decoder) Uint8() (uint8, error) {
	value, err := d.Raw(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}

func (d *Decoder) Uint16() (uint16, error) {
	value, err := d.Raw(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(value), nil
}

func (d *Decoder) Uint32() (uint32, error) {
	value, err := d.Raw(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(value), nil
}

func (d *Decoder) Uint64() (uint64, error) {
	value, err := d.Raw(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(value), nil
}

func (d *Decoder) Int64() (int64, error) {
	value, err := d.Uint64()
	return int64(value), err
}

func (d *Decoder) Bytes(limit int) ([]byte, error) {
	length, err := d.Uint32()
	if err != nil {
		return nil, err
	}
	if uint64(length) > uint64(^uint(0)>>1) || (limit > 0 && length > uint32(limit)) {
		return nil, fmt.Errorf("FSKit protocol byte field %d exceeds limit %d", length, limit)
	}
	return d.Raw(int(length))
}

func (d *Decoder) String(limit int) (string, error) {
	value, err := d.Bytes(limit)
	if err != nil {
		return "", err
	}
	return string(value), nil
}

func (d *Decoder) Time() (time.Time, error) {
	seconds, err := d.Int64()
	if err != nil {
		return time.Time{}, err
	}
	nanoseconds, err := d.Uint32()
	if err != nil {
		return time.Time{}, err
	}
	if seconds == 0 && nanoseconds == 0 {
		return time.Time{}, nil
	}
	if nanoseconds >= 1_000_000_000 {
		return time.Time{}, errors.New("invalid FSKit protocol timestamp")
	}
	return time.Unix(seconds, int64(nanoseconds)), nil
}

func (d *Decoder) Entry() (Entry, error) {
	var entry Entry
	var err error
	if entry.Path, err = d.String(1 << 20); err != nil {
		return Entry{}, err
	}
	if entry.Name, err = d.String(4096); err != nil {
		return Entry{}, err
	}
	if entry.NodeID, err = d.Uint64(); err != nil {
		return Entry{}, err
	}
	if entry.ParentID, err = d.Uint64(); err != nil {
		return Entry{}, err
	}
	typeValue, err := d.Uint8()
	if err != nil {
		return Entry{}, err
	}
	entry.Type = EntryType(typeValue)
	if entry.Mode, err = d.Uint32(); err != nil {
		return Entry{}, err
	}
	if entry.UID, err = d.Uint32(); err != nil {
		return Entry{}, err
	}
	if entry.GID, err = d.Uint32(); err != nil {
		return Entry{}, err
	}
	if entry.Size, err = d.Uint64(); err != nil {
		return Entry{}, err
	}
	if entry.AllocSize, err = d.Uint64(); err != nil {
		return Entry{}, err
	}
	if entry.ModTime, err = d.Time(); err != nil {
		return Entry{}, err
	}
	if entry.ChangeTime, err = d.Time(); err != nil {
		return Entry{}, err
	}
	if entry.AccessTime, err = d.Time(); err != nil {
		return Entry{}, err
	}
	if entry.NamespaceID, err = d.Uint64(); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

func (d *Decoder) StatFS() (StatFS, error) {
	var stat StatFS
	var err error
	if stat.BlockSize, err = d.Uint32(); err != nil {
		return StatFS{}, err
	}
	if stat.IOSize, err = d.Uint32(); err != nil {
		return StatFS{}, err
	}
	if stat.TotalBytes, err = d.Uint64(); err != nil {
		return StatFS{}, err
	}
	if stat.AvailableBytes, err = d.Uint64(); err != nil {
		return StatFS{}, err
	}
	if stat.FreeBytes, err = d.Uint64(); err != nil {
		return StatFS{}, err
	}
	if stat.UsedBytes, err = d.Uint64(); err != nil {
		return StatFS{}, err
	}
	if stat.TotalFiles, err = d.Uint64(); err != nil {
		return StatFS{}, err
	}
	if stat.FreeFiles, err = d.Uint64(); err != nil {
		return StatFS{}, err
	}
	return stat, nil
}

func (d *Decoder) Done() error {
	if d.remaining() != 0 {
		return fmt.Errorf("FSKit protocol payload has %d trailing bytes", d.remaining())
	}
	return nil
}
