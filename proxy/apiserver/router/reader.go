/*
Copyright 2021 The Kruise Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package router

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"

	"golang.org/x/net/http2"
	"k8s.io/klog/v2"
)

const (
	maxBufferBytes = 16 * 1024 * 1024
)

var ErrObjectTooLarge = fmt.Errorf("object to decode was longer than maximum allowed size")

type ioReader struct {
	reader    io.ReadCloser
	resetRead bool
}

func (r *ioReader) readOnce() ([]byte, error) {
	body, err := ioutil.ReadAll(r.reader)
	switch err.(type) {
	case nil:
	case http2.StreamError:
		// This is trying to catch the scenario that the server may close the connection when sending the
		// response body. This can be caused by server timeout due to a slow network connection.
		// TODO: Add test for this. Steps may be:
		// 1. client-go (or kubectl) sends a GET request.
		// 2. Apiserver sends back the headers and then part of the body
		// 3. Apiserver closes connection.
		// 4. client-go should catch this and return an error.
		klog.V(2).Infof("Stream error %#v when reading response body, may be caused by closed connection.", err)
		return nil, fmt.Errorf("stream error when reading response body, may be caused by closed connection. Please retry. Original error: %v", err)
	default:
		klog.Errorf("Unexpected error when reading response body: %v", err)
		return nil, fmt.Errorf("unexpected error when reading response body. Please retry. Original error: %v", err)
	}

	return body, nil
}

func (r *ioReader) readStreaming(buf *bytes.Buffer) (n int, err error) {
	base := 0
	for {
		n, err := r.reader.Read(buf.Bytes()[base:buf.Cap()])
		if err == io.ErrShortBuffer {
			if n == 0 {
				return base, fmt.Errorf("got short buffer with n=0, base=%d, cap=%d", base, buf.Cap())
			}
			if r.resetRead {
				continue
			}
			// double the buffer size up to maxBytes
			if len(buf.Bytes()[:buf.Cap()]) < maxBufferBytes {
				base += n
				buf.Grow(len(buf.Bytes()[:buf.Cap()]))
				continue
			}
			// must read the rest of the frame (until we stop getting ErrShortBuffer)
			r.resetRead = true
			base = 0
			return base, ErrObjectTooLarge
		}
		if err != nil {
			return base, err
		}
		if r.resetRead {
			// now that we have drained the large read, continue
			r.resetRead = false
			continue
		}
		base += n
		break
	}
	return base, nil
}

func (r *ioReader) close() {
	_ = r.reader.Close()
}
