package fskitproto

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const DescriptorFilename = "descriptor.bin"

type StatusError struct {
	Operation Op
	Errno     syscall.Errno
}

func (e StatusError) Error() string {
	return fmt.Sprintf("FSKit operation %d failed: %s", e.Operation, e.Errno)
}

type Client struct {
	mu         sync.Mutex
	connection net.Conn
	generation uint64
	maxPayload uint32
	nextID     uint64
	timeout    time.Duration
}

func DialResource(resourcePath string, timeout time.Duration) (*Client, error) {
	return DialResourceContext(context.Background(), resourcePath, timeout)
}

func DialResourceContext(ctx context.Context, resourcePath string, timeout time.Duration) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	descriptorPath, err := ResourceDescriptorPath(resourcePath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(descriptorPath)
	if err != nil {
		return nil, fmt.Errorf("read FSKit resource: %w", err)
	}
	descriptor, err := DecodeDescriptor(data)
	if err != nil {
		return nil, err
	}
	return DialContext(ctx, descriptor, timeout)
}

func ResourceDescriptorPath(resourcePath string) (string, error) {
	if !filepath.IsAbs(resourcePath) {
		return "", errors.New("absolute FSKit resource path is required")
	}
	resourcePath = filepath.Clean(resourcePath)
	if UsesDirectoryResource(resourcePath) {
		return filepath.Join(resourcePath, DescriptorFilename), nil
	}
	_, err := os.Stat(resourcePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return resourcePath, nil
}

func UsesDirectoryResource(resourcePath string) bool {
	if info, err := os.Stat(filepath.Clean(resourcePath)); err == nil {
		return info.IsDir()
	}
	return !strings.EqualFold(filepath.Ext(resourcePath), ".bin")
}

func Dial(descriptor Descriptor, timeout time.Duration) (*Client, error) {
	return DialContext(context.Background(), descriptor, timeout)
}

func DialContext(ctx context.Context, descriptor Descriptor, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(dialCtx, "unix", descriptor.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to FSKit daemon: %w", err)
	}
	client := &Client{connection: connection, generation: descriptor.Generation, maxPayload: DefaultMaxPayload, nextID: 1, timeout: timeout}
	encoder := NewEncoder(len(descriptor.Token) + 4)
	encoder.Bytes(descriptor.Token)
	response, err := client.callContextLocked(dialCtx, OpHello, 0, encoder.Data())
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	decoder := NewDecoder(response)
	maxPayload, err := decoder.Uint32()
	if err != nil || maxPayload < 4096 {
		_ = connection.Close()
		return nil, errors.New("invalid FSKit daemon hello response")
	}
	if _, err := decoder.Uint64(); err != nil || decoder.Done() != nil {
		_ = connection.Close()
		return nil, errors.New("invalid FSKit daemon hello namespace response")
	}
	client.maxPayload = maxPayload
	return client, nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connection == nil {
		return nil
	}
	err := c.connection.Close()
	c.connection = nil
	return err
}

func (c *Client) Call(operation Op, payload []byte) ([]byte, error) {
	return c.CallContext(context.Background(), operation, payload)
}

func (c *Client) CallContext(ctx context.Context, operation Op, payload []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.callContextLocked(ctx, operation, c.generation, payload)
}

func (c *Client) callContextLocked(ctx context.Context, operation Op, generation uint64, payload []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.connection == nil {
		return nil, net.ErrClosed
	}
	timeout := c.timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	connection := c.connection
	if err := connection.SetDeadline(deadline); err != nil {
		_ = connection.Close()
		c.connection = nil
		return nil, err
	}
	canceled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = connection.SetDeadline(time.Now())
		close(canceled)
	})
	response, err := c.callLocked(operation, generation, payload)
	if !stop() {
		<-canceled
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	var status StatusError
	if err != nil && !errors.As(err, &status) {
		// A partial frame cannot be reused: a late response would otherwise be
		// mistaken for the next request. Closing also releases blocked peers.
		_ = connection.Close()
		c.connection = nil
	} else if clearErr := connection.SetDeadline(time.Time{}); clearErr != nil {
		_ = connection.Close()
		c.connection = nil
		return nil, clearErr
	}
	return response, err
}

func (c *Client) callLocked(operation Op, generation uint64, payload []byte) ([]byte, error) {
	if c.connection == nil {
		return nil, net.ErrClosed
	}
	requestID := c.nextID
	c.nextID++
	if err := WriteFrame(c.connection, Frame{
		Kind: KindRequest, Op: operation, RequestID: requestID, Generation: generation, Payload: payload,
	}, c.maxPayload); err != nil {
		return nil, err
	}
	response, err := ReadFrame(c.connection, c.maxPayload)
	if err != nil {
		return nil, err
	}
	if response.Kind != KindResponse || response.Op != operation || response.RequestID != requestID || response.Generation != c.generation {
		return nil, errors.New("mismatched FSKit daemon response")
	}
	if response.Status != 0 {
		return nil, StatusError{Operation: operation, Errno: syscall.Errno(response.Status)}
	}
	return response.Payload, nil
}

func ErrorNumber(err error) syscall.Errno {
	var status StatusError
	if errors.As(err, &status) {
		return status.Errno
	}
	return syscall.EIO
}
