package http2

import (
	"errors"
	"testing"
)

func TestErrorCode_String(t *testing.T) {
	tests := []struct {
		name string
		e    ErrorCode
		want string
	}{
		{"NoError", ErrCodeNoError, "NO_ERROR"},
		{"ProtocolError", ErrCodeProtocolError, "PROTOCOL_ERROR"},
		{"InternalError", ErrCodeInternalError, "INTERNAL_ERROR"},
		{"FlowControlError", ErrCodeFlowControlError, "FLOW_CONTROL_ERROR"},
		{"SettingsTimeout", ErrCodeSettingsTimeout, "SETTINGS_TIMEOUT"},
		{"StreamClosed", ErrCodeStreamClosed, "STREAM_CLOSED"},
		{"FrameSizeError", ErrCodeFrameSizeError, "FRAME_SIZE_ERROR"},
		{"RefusedStream", ErrCodeRefusedStream, "REFUSED_STREAM"},
		{"Cancel", ErrCodeCancel, "CANCEL"},
		{"CompressionError", ErrCodeCompressionError, "COMPRESSION_ERROR"},
		{"ConnectError", ErrCodeConnectError, "CONNECT_ERROR"},
		{"EnhanceYourCalm", ErrCodeEnhanceYourCalm, "ENHANCE_YOUR_CALM"},
		{"InadequateSecurity", ErrCodeInadequateSecurity, "INADEQUATE_SECURITY"},
		{"HTTP11Required", ErrCodeHTTP11Required, "HTTP_1_1_REQUIRED"},
		{"UnknownErrorCode", ErrorCode(0xff), "UNKNOWN_ERROR_CODE_255"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.e.String(); got != tt.want {
				t.Errorf("ErrorCode.String() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStreamError(t *testing.T) {
	baseErr := errors.New("underlying cause")

	tests := []struct {
		name       string
		streamID   uint32
		code       ErrorCode
		msg        string
		cause      error
		wantError  string
		checkCause bool
	}{
		{
			name:      "simple stream error",
			streamID:  1,
			code:      ErrCodeProtocolError,
			msg:       "invalid frame",
			wantError: "stream error on stream 1: invalid frame (code PROTOCOL_ERROR, 1)",
		},
		{
			name:       "stream error with cause",
			streamID:   3,
			code:       ErrCodeInternalError,
			msg:        "handler panic",
			cause:      baseErr,
			wantError:  "stream error on stream 3: handler panic (code INTERNAL_ERROR, 2): underlying cause",
			checkCause: true,
		},
		{
			name:      "stream error with zero stream ID",
			streamID:  0,
			code:      ErrCodeStreamClosed,
			msg:       "stream already closed",
			wantError: "stream error on stream 0: stream already closed (code STREAM_CLOSED, 5)",
		},
		{
			name:      "stream error with empty message",
			streamID:  5,
			code:      ErrCodeCancel,
			msg:       "", // Empty message
			wantError: "stream error on stream 5:  (code CANCEL, 8)",
			// cause is nil, checkCause defaults to false
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err *StreamError
			if tt.cause != nil {
				err = NewStreamErrorWithCause(tt.streamID, tt.code, tt.msg, tt.cause)
			} else {
				err = NewStreamError(tt.streamID, tt.code, tt.msg)
			}

			if gotError := err.Error(); gotError != tt.wantError {
				t.Errorf("StreamError.Error() got = %q, want %q", gotError, tt.wantError)
			}

			if tt.checkCause {
				if gotCause := errors.Unwrap(err); gotCause != tt.cause {
					t.Errorf("StreamError.Unwrap() got = %v, want %v", gotCause, tt.cause)
				}
			} else {
				if gotCause := errors.Unwrap(err); gotCause != nil {
					t.Errorf("StreamError.Unwrap() got = %v, want nil", gotCause)
				}
			}

			if err.StreamID != tt.streamID {
				t.Errorf("StreamError.StreamID got = %d, want %d", err.StreamID, tt.streamID)
			}
			if err.Code != tt.code {
				t.Errorf("StreamError.Code got = %s, want %s", err.Code, tt.code)
			}
			if err.Msg != tt.msg {
				t.Errorf("StreamError.Msg got = %q, want %q", err.Msg, tt.msg)
			}
		})
	}
}

func TestConnectionError(t *testing.T) {
	baseErr := errors.New("underlying connection issue")

	tests := []struct {
		name          string
		lastStreamID  uint32
		code          ErrorCode
		msg           string
		cause         error
		debugData     []byte
		wantError     string
		wantDebugData []byte
		checkCause    bool
	}{
		{
			name:         "simple connection error",
			lastStreamID: 15,
			code:         ErrCodeProtocolError,
			msg:          "bad magic",
			wantError:    "connection error: bad magic (last_stream_id 15, code PROTOCOL_ERROR, 1)",
		},
		{
			name:         "connection error with cause",
			lastStreamID: 0,
			code:         ErrCodeInternalError,
			msg:          "config load failed",
			cause:        baseErr,
			wantError:    "connection error: config load failed (last_stream_id 0, code INTERNAL_ERROR, 2): underlying connection issue",
			checkCause:   true,
		},
		{
			name:          "connection error with debug data",
			lastStreamID:  7,
			code:          ErrCodeEnhanceYourCalm,
			msg:           "too many pings",
			debugData:     []byte("calm down bro"),
			wantError:     "connection error: too many pings (last_stream_id 7, code ENHANCE_YOUR_CALM, 11)",
			wantDebugData: []byte("calm down bro"),
		},
		{
			name:          "connection error with cause and debug data",
			lastStreamID:  3,
			code:          ErrCodeConnectError,
			msg:           "TLS handshake failed",
			cause:         baseErr,
			debugData:     []byte("TLS alert: bad_certificate"),
			wantError:     "connection error: TLS handshake failed (last_stream_id 3, code CONNECT_ERROR, 10): underlying connection issue",
			wantDebugData: []byte("TLS alert: bad_certificate"),
			checkCause:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err *ConnectionError
			if tt.cause != nil {
				err = NewConnectionErrorWithCause(tt.code, tt.msg, tt.cause)
			} else {
				err = NewConnectionError(tt.code, tt.msg)
			}
			err.LastStreamID = tt.lastStreamID
			if tt.debugData != nil {
				err.DebugData = tt.debugData
			}

			if gotError := err.Error(); gotError != tt.wantError {
				t.Errorf("ConnectionError.Error() got = %q, want %q", gotError, tt.wantError)
			}

			if tt.checkCause {
				if gotCause := errors.Unwrap(err); gotCause != tt.cause {
					t.Errorf("ConnectionError.Unwrap() got = %v, want %v", gotCause, tt.cause)
				}
			} else {
				if gotCause := errors.Unwrap(err); gotCause != nil {
					t.Errorf("ConnectionError.Unwrap() got = %v, want nil", gotCause)
				}
			}

			if err.LastStreamID != tt.lastStreamID {
				t.Errorf("ConnectionError.LastStreamID got = %d, want %d", err.LastStreamID, tt.lastStreamID)
			}
			if err.Code != tt.code {
				t.Errorf("ConnectionError.Code got = %s, want %s", err.Code, tt.code)
			}
			if err.Msg != tt.msg {
				t.Errorf("ConnectionError.Msg got = %q, want %q", err.Msg, tt.msg)
			}
			if string(err.DebugData) != string(tt.wantDebugData) { // Compare string representation for easier diff
				t.Errorf("ConnectionError.DebugData got = %q, want %q", string(err.DebugData), string(tt.wantDebugData))
			}
		})
	}
}

func TestGenerateRSTStreamFrame(t *testing.T) {
	underlyingErr := errors.New("some internal problem")

	tests := []struct {
		name              string
		streamIDArg       uint32
		errCodeArg        ErrorCode
		errArg            error
		expectedStreamID  uint32
		expectedErrorCode ErrorCode
	}{
		{
			name:              "direct error code",
			streamIDArg:       1,
			errCodeArg:        ErrCodeProtocolError,
			errArg:            nil,
			expectedStreamID:  1,
			expectedErrorCode: ErrCodeProtocolError,
		},
		{
			name:              "StreamError provided, args ignored",
			streamIDArg:       1, // This should be overridden by StreamError's StreamID if StreamError.StreamID is non-zero
			errCodeArg:        ErrCodeProtocolError,
			errArg:            &StreamError{StreamID: 3, Code: ErrCodeStreamClosed, Msg: "closed"},
			expectedStreamID:  3,
			expectedErrorCode: ErrCodeStreamClosed,
		},
		{
			name:              "StreamError provided with cause",
			streamIDArg:       5, // Overridden
			errCodeArg:        ErrCodeProtocolError,
			errArg:            NewStreamErrorWithCause(7, ErrCodeCancel, "cancelled by client", underlyingErr),
			expectedStreamID:  7,
			expectedErrorCode: ErrCodeCancel,
		},
		{
			name:              "StreamError with StreamID 0, uses streamIDArg",
			streamIDArg:       9, // This should be used
			errCodeArg:        ErrCodeProtocolError,
			errArg:            &StreamError{StreamID: 0, Code: ErrCodeRefusedStream, Msg: "refused"},
			expectedStreamID:  9,
			expectedErrorCode: ErrCodeRefusedStream,
		},
		{
			name:              "Non-StreamError error, uses args",
			streamIDArg:       11,
			errCodeArg:        ErrCodeInternalError,
			errArg:            underlyingErr, // A generic error
			expectedStreamID:  11,
			expectedErrorCode: ErrCodeInternalError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame := GenerateRSTStreamFrame(tt.streamIDArg, tt.errCodeArg, tt.errArg)

			if frame.Header().Type != FrameRSTStream {
				t.Errorf("GenerateRSTStreamFrame() Type got = %v, want %v", frame.Header().Type, FrameRSTStream)
			}
			if frame.Header().StreamID != tt.expectedStreamID {
				t.Errorf("GenerateRSTStreamFrame() StreamID got = %d, want %d", frame.Header().StreamID, tt.expectedStreamID)
			}
			if frame.Header().Length != 4 {
				t.Errorf("GenerateRSTStreamFrame() Length got = %d, want %d", frame.Header().Length, 4)
			}
			if frame.Header().Flags != 0 {
				t.Errorf("GenerateRSTStreamFrame() Flags got = %d, want %d", frame.Header().Flags, 0)
			}
			if frame.ErrorCode != tt.expectedErrorCode {
				t.Errorf("GenerateRSTStreamFrame() ErrorCode got = %v, want %v", frame.ErrorCode, tt.expectedErrorCode)
			}
		})
	}
}

func TestGenerateGoAwayFrame(t *testing.T) {
	underlyingConnErr := errors.New("some connection problem")

	tests := []struct {
		name                 string
		lastStreamIDArg      uint32
		errCodeArg           ErrorCode
		debugStrArg          string
		errArg               error
		expectedLastStreamID uint32
		expectedErrorCode    ErrorCode
		expectedDebugDataStr string // Compare as string for easier assertion
	}{
		{
			name:                 "direct error code and debug string",
			lastStreamIDArg:      100,
			errCodeArg:           ErrCodeNoError,
			debugStrArg:          "shutdown",
			errArg:               nil,
			expectedLastStreamID: 100,
			expectedErrorCode:    ErrCodeNoError,
			expectedDebugDataStr: "shutdown",
		},
		{
			name:                 "ConnectionError provided, args ignored for code and lastStreamID",
			lastStreamIDArg:      100, // This should be overridden by ConnectionError's LastStreamID
			errCodeArg:           ErrCodeNoError,
			debugStrArg:          "generic shutdown",
			errArg:               &ConnectionError{LastStreamID: 200, Code: ErrCodeProtocolError, Msg: "bad handshake"},
			expectedLastStreamID: 200,
			expectedErrorCode:    ErrCodeProtocolError,
			expectedDebugDataStr: "generic shutdown", // With new logic, debugStrArg ("generic shutdown") takes precedence over ce.Msg ("bad handshake") when ce.DebugData is empty.
		},
		{
			name:                 "ConnectionError with its own DebugData",
			lastStreamIDArg:      100,
			errCodeArg:           ErrCodeNoError,
			debugStrArg:          "this will be ignored",
			errArg:               &ConnectionError{LastStreamID: 250, Code: ErrCodeInternalError, Msg: "server dying", DebugData: []byte("custom debug")},
			expectedLastStreamID: 250,
			expectedErrorCode:    ErrCodeInternalError,
			expectedDebugDataStr: "custom debug",
		},
		{
			name:                 "ConnectionError with cause",
			lastStreamIDArg:      100,
			errCodeArg:           ErrCodeNoError,
			debugStrArg:          "",
			errArg:               NewConnectionErrorWithCause(ErrCodeConnectError, "failed to connect", underlyingConnErr),
			expectedLastStreamID: 0, // NewConnectionError doesn't set LastStreamID, so it remains the default for ConnectionError (0)
			expectedErrorCode:    ErrCodeConnectError,
			expectedDebugDataStr: "failed to connect", // Msg used as debug data
		},
		{
			name:            "ConnectionError with cause, LastStreamID set on struct",
			lastStreamIDArg: 100, // This will be used if ce.LastStreamID is not directly assigned another value in the test case
			errCodeArg:      ErrCodeNoError,
			debugStrArg:     "",
			errArg: func() error {
				ce := NewConnectionErrorWithCause(ErrCodeEnhanceYourCalm, "calm down", underlyingConnErr)
				ce.LastStreamID = 300 // Explicitly set
				return ce
			}(),
			expectedLastStreamID: 300,
			expectedErrorCode:    ErrCodeEnhanceYourCalm,
			expectedDebugDataStr: "calm down",
		},

		{
			name:                 "ConnectionError with empty DebugData and Msg, uses debugStrArg",
			lastStreamIDArg:      400,            // Will be overridden by ce.LastStreamID
			errCodeArg:           ErrCodeNoError, // Will be overridden by ce.Code
			debugStrArg:          "fallback debug from arg",
			errArg:               &ConnectionError{LastStreamID: 450, Code: ErrCodeRefusedStream, Msg: "", DebugData: nil},
			expectedLastStreamID: 450,
			expectedErrorCode:    ErrCodeRefusedStream,
			expectedDebugDataStr: "fallback debug from arg",
		},
		{
			name:                 "Non-ConnectionError error, uses args",
			lastStreamIDArg:      50,
			errCodeArg:           ErrCodeFrameSizeError,
			debugStrArg:          "frame too big",
			errArg:               underlyingConnErr, // A generic error
			expectedLastStreamID: 50,
			expectedErrorCode:    ErrCodeFrameSizeError,
			expectedDebugDataStr: "frame too big",
		},
		{
			name:                 "ConnectionError, debugStrArg provided, ce.DebugData empty, ce.Msg exists",
			lastStreamIDArg:      10,
			errCodeArg:           ErrCodeHTTP11Required,
			debugStrArg:          "explicit debug string", // This should be preferred over ce.Msg
			errArg:               &ConnectionError{LastStreamID: 20, Code: ErrCodeSettingsTimeout, Msg: "settings timeout msg"},
			expectedLastStreamID: 20,
			expectedErrorCode:    ErrCodeSettingsTimeout,
			expectedDebugDataStr: "explicit debug string",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame := GenerateGoAwayFrame(tt.lastStreamIDArg, tt.errCodeArg, tt.debugStrArg, tt.errArg)

			if frame.Header().Type != FrameGoAway {
				t.Errorf("GenerateGoAwayFrame() Type got = %v, want %v", frame.Header().Type, FrameGoAway)
			}
			if frame.Header().StreamID != 0 {
				t.Errorf("GenerateGoAwayFrame() StreamID got = %d, want %d", frame.Header().StreamID, 0)
			}
			expectedLength := uint32(8 + len(tt.expectedDebugDataStr))
			if frame.Header().Length != expectedLength {
				t.Errorf("GenerateGoAwayFrame() Length got = %d, want %d", frame.Header().Length, expectedLength)
			}
			if frame.Header().Flags != 0 {
				t.Errorf("GenerateGoAwayFrame() Flags got = %d, want %d", frame.Header().Flags, 0)
			}
			if frame.LastStreamID != tt.expectedLastStreamID {
				t.Errorf("GenerateGoAwayFrame() LastStreamID got = %d, want %d", frame.LastStreamID, tt.expectedLastStreamID)
			}
			if frame.ErrorCode != tt.expectedErrorCode {
				t.Errorf("GenerateGoAwayFrame() ErrorCode got = %v, want %v", frame.ErrorCode, tt.expectedErrorCode)
			}
			if string(frame.AdditionalDebugData) != tt.expectedDebugDataStr {
				t.Errorf("GenerateGoAwayFrame() AdditionalDebugData got = %q, want %q", string(frame.AdditionalDebugData), tt.expectedDebugDataStr)
			}
		})
	}
}
