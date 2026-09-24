package infoflow

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
)

// Frame wire field numbers. Kept in sync with the official
// @baidu/infoflow-sdk-nodejs proto/frame.proto. Go does not have an official
// generated package, so this adapter encodes/decodes the tiny protobuf frame in
// place with protowire. The schema is deliberately frozen:
//
//	message Frame {
//	  uint64 seq_id = 1;
//	  string log_id = 2;
//	  int32  service = 3;
//	  int32  method = 4;
//	  repeated Header headers = 5;
//	  bytes  payload = 6;
//	}
const (
	frameFieldSeqID   protowire.Number = 1
	frameFieldLogID   protowire.Number = 2
	frameFieldService protowire.Number = 3
	frameFieldMethod  protowire.Number = 4
	frameFieldHeaders protowire.Number = 5
	frameFieldPayload protowire.Number = 6
)

const (
	headerFieldKey   protowire.Number = 1
	headerFieldValue protowire.Number = 2
)

// FrameMethod mirrors the SDk's frame type enum: CONTROL=0, DATA=1,
// REQUEST=2, RESPONSE=3.
type FrameMethod int32

const (
	FrameControl FrameMethod = 0
	FrameData    FrameMethod = 1
	FrameRequest FrameMethod = 2
	FrameResponse FrameMethod = 3
)

// Header is a single repeated {key,value} pair in a Frame.
type Header struct {
	Key   string
	Value string
}

// Frame is the binary frame exchanged over the InfoFlow WebSocket after the
// two-phase endpoint fetch.
type Frame struct {
	SeqID   uint64
	LogID   string
	Service int32
	Method  FrameMethod
	Headers []Header
	Payload []byte
}

// marshalFrame encodes the small InfoFlow protobuf frame.
func marshalFrame(f Frame) ([]byte, error) {
	buf := make([]byte, 0, 256)
	if f.SeqID != 0 {
		buf = protowire.AppendTag(buf, frameFieldSeqID, protowire.VarintType)
		buf = protowire.AppendVarint(buf, f.SeqID)
	}
	if f.LogID != "" {
		buf = protowire.AppendTag(buf, frameFieldLogID, protowire.BytesType)
		buf = protowire.AppendString(buf, f.LogID)
	}
	if f.Service != 0 {
		buf = protowire.AppendTag(buf, frameFieldService, protowire.VarintType)
		buf = protowire.AppendVarint(buf, uint64(uint32(f.Service)))
	}
	if f.Method != 0 {
		buf = protowire.AppendTag(buf, frameFieldMethod, protowire.VarintType)
		buf = protowire.AppendVarint(buf, uint64(uint32(f.Method)))
	}
	for _, h := range f.Headers {
		buf = protowire.AppendTag(buf, frameFieldHeaders, protowire.BytesType)
		buf = protowire.AppendBytes(buf, encodeHeader(h))
	}
	if len(f.Payload) != 0 {
		buf = protowire.AppendTag(buf, frameFieldPayload, protowire.BytesType)
		buf = protowire.AppendBytes(buf, f.Payload)
	}
	return buf, nil
}

// unmarshalFrame decodes a wire-format InfoFlow frame.
func unmarshalFrame(data []byte) (Frame, error) {
	var f Frame
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return Frame{}, protowire.ParseError(n)
		}
		data = data[n:]
		switch num {
		case frameFieldSeqID:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return Frame{}, protowire.ParseError(n)
			}
			f.SeqID = v
			data = data[n:]
		case frameFieldLogID:
			v, n := protowire.ConsumeString(data)
			if n < 0 {
				return Frame{}, protowire.ParseError(n)
			}
			f.LogID = v
			data = data[n:]
		case frameFieldService:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return Frame{}, protowire.ParseError(n)
			}
			f.Service = int32(v)
			data = data[n:]
		case frameFieldMethod:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return Frame{}, protowire.ParseError(n)
			}
			f.Method = FrameMethod(v)
			data = data[n:]
		case frameFieldHeaders:
			if typ != protowire.BytesType {
				return Frame{}, fmt.Errorf("infoflow: unexpected header wire type %d", typ)
			}
			raw, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return Frame{}, protowire.ParseError(n)
			}
			h, err := decodeHeader(raw)
			if err != nil {
				return Frame{}, err
			}
			f.Headers = append(f.Headers, h)
			data = data[n:]
		case frameFieldPayload:
			if typ != protowire.BytesType {
				return Frame{}, fmt.Errorf("infoflow: unexpected payload wire type %d", typ)
			}
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return Frame{}, protowire.ParseError(n)
			}
			f.Payload = append([]byte(nil), v...)
			data = data[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return Frame{}, protowire.ParseError(n)
			}
			data = data[n:]
		}
	}
	return f, nil
}

func encodeHeader(h Header) []byte {
	buf := protowire.AppendTag(nil, headerFieldKey, protowire.BytesType)
	buf = protowire.AppendString(buf, h.Key)
	buf = protowire.AppendTag(buf, headerFieldValue, protowire.BytesType)
	buf = protowire.AppendString(buf, h.Value)
	return buf
}

func decodeHeader(data []byte) (Header, error) {
	var h Header
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return Header{}, protowire.ParseError(n)
		}
		data = data[n:]
		switch num {
		case headerFieldKey:
			if typ != protowire.BytesType {
				return Header{}, errors.New("infoflow: header key is not string")
			}
			v, n := protowire.ConsumeString(data)
			if n < 0 {
				return Header{}, protowire.ParseError(n)
			}
			h.Key = v
			data = data[n:]
		case headerFieldValue:
			if typ != protowire.BytesType {
				return Header{}, errors.New("infoflow: header value is not string")
			}
			v, n := protowire.ConsumeString(data)
			if n < 0 {
				return Header{}, protowire.ParseError(n)
			}
			h.Value = v
			data = data[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return Header{}, protowire.ParseError(n)
			}
			data = data[n:]
		}
	}
	return h, nil
}

// header returns the first header value for key.
func headerValue(headers []Header, key string) string {
	for _, h := range headers {
		if h.Key == key {
			return h.Value
		}
	}
	return ""
}
