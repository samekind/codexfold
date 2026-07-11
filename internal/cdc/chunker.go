package cdc

import (
	"crypto/sha256"
	"fmt"
)

type Options struct {
	MinBytes     int64
	AverageBytes int64
	MaxBytes     int64
}

type Chunk struct {
	Data   []byte
	Digest [sha256.Size]byte
}

type Chunker struct {
	options Options
	mask    uint64
	gear    uint64
	chunk   []byte
	emit    func(Chunk) error
}

var dedupGearTable = buildDedupGearTable()

func New(options Options, emit func(Chunk) error) (*Chunker, error) {
	if options.MinBytes <= 0 {
		options.MinBytes = 64 * 1024
	}
	if options.AverageBytes <= 0 {
		options.AverageBytes = 256 * 1024
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = 1024 * 1024
	}
	if options.MinBytes > options.AverageBytes || options.AverageBytes > options.MaxBytes {
		return nil, fmt.Errorf("invalid CDC bounds: require min <= average <= max")
	}
	if options.AverageBytes&(options.AverageBytes-1) != 0 {
		return nil, fmt.Errorf("invalid CDC average %d: must be a power of two", options.AverageBytes)
	}
	if emit == nil {
		return nil, fmt.Errorf("CDC emit callback is required")
	}
	return &Chunker{
		options: options,
		mask:    uint64(options.AverageBytes - 1),
		chunk:   make([]byte, 0, options.MaxBytes),
		emit:    emit,
	}, nil
}

func (c *Chunker) Write(data []byte) error {
	for _, value := range data {
		c.chunk = append(c.chunk, value)
		c.gear = (c.gear << 1) + dedupGearTable[value]
		size := int64(len(c.chunk))
		if size >= c.options.MaxBytes || (size >= c.options.MinBytes && c.gear&c.mask == 0) {
			if err := c.emitChunk(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Chunker) Finish() error {
	if len(c.chunk) == 0 {
		return nil
	}
	return c.emitChunk()
}

func (c *Chunker) emitChunk() error {
	digest := sha256.Sum256(c.chunk)
	if err := c.emit(Chunk{Data: c.chunk, Digest: digest}); err != nil {
		return err
	}
	c.chunk = c.chunk[:0]
	c.gear = 0
	return nil
}

func buildDedupGearTable() [256]uint64 {
	var table [256]uint64
	state := uint64(0x6a09e667f3bcc909)
	for index := range table {
		state += 0x9e3779b97f4a7c15
		value := state
		value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
		value = (value ^ (value >> 27)) * 0x94d049bb133111eb
		table[index] = value ^ (value >> 31)
	}
	return table
}
