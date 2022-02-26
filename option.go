package connect

import (
	"strings"

	"github.com/bufbuild/connect/codec"
)

// Option implements both ClientOption and HandlerOption, so it can be applied
// both client-side and server-side.
type Option interface {
	ClientOption
	HandlerOption
}

type replaceProcedurePrefixOption struct {
	prefix      string
	replacement string
}

// WithReplaceProcedurePrefix changes the URL used to call a procedure.
// Typically, generated code sets the procedure name: for example, a protobuf
// procedure's name and URL is composed from the fully-qualified protobuf
// package name, the service name, and the method name. This option replaces a
// prefix of the procedure name with another static string. Using this option
// is usually a bad idea, but it's occasionally necessary to prevent protobuf
// package collisions. (For example, connect uses this option to serve the
// health and reflection APIs without generating runtime conflicts with
// grpc-go.)
//
// WithReplaceProcedurePrefix doesn't change the data exposed by the reflection
// API. To prevent inconsistencies between the reflection data and the actual
// service URL, using this option disables reflection for the modified service
// (though other services can still be introspected).
func WithReplaceProcedurePrefix(prefix, replacement string) Option {
	return &replaceProcedurePrefixOption{
		prefix:      prefix,
		replacement: replacement,
	}
}

func (o *replaceProcedurePrefixOption) applyToClient(config *clientConfiguration) {
	config.Procedure = o.transform(config.Procedure)
}

func (o *replaceProcedurePrefixOption) applyToHandler(config *handlerConfiguration) {
	config.Procedure = o.transform(config.Procedure)
	config.RegistrationName = "" // disable reflection
}

func (o *replaceProcedurePrefixOption) transform(name string) string {
	if !strings.HasPrefix(name, o.prefix) {
		return name
	}
	return o.replacement + strings.TrimPrefix(name, o.prefix)
}

type readMaxBytesOption struct {
	Max int64
}

// WithReadMaxBytes limits the performance impact of pathologically large
// messages sent by the other party. For handlers, WithReadMaxBytes limits the size
// of message that the client can send. For clients, WithReadMaxBytes limits the
// size of message that the server can respond with. Limits are applied before
// decompression and apply to each protobuf message, not to the stream as a
// whole.
//
// Setting WithReadMaxBytes to zero allows any message size. Both clients and
// handlers default to allowing any request size.
func WithReadMaxBytes(n int64) Option {
	return &readMaxBytesOption{n}
}

func (o *readMaxBytesOption) applyToClient(config *clientConfiguration) {
	config.MaxResponseBytes = o.Max
}

func (o *readMaxBytesOption) applyToHandler(config *handlerConfiguration) {
	config.MaxRequestBytes = o.Max
}

type codecOption struct {
	Name  string
	Codec codec.Codec
}

// WithCodec registers a serialization method with a client or handler.
// Registering a codec with an empty name is a no-op.
//
// Typically, generated code automatically supplies this option with the
// appropriate codec(s). For example, handlers generated from protobuf schemas
// using protoc-gen-go-connect automatically register binary and JSON codecs.
// Users with more specialized needs may override the default codecs by
// registering a new codec under the same name.
//
// Handlers may have multiple codecs registered, and use whichever the client
// chooses. Clients may only have a single codec.
//
// When registering protocol buffer codecs, take care to use connect's
// protobuf.NameBinary ("protobuf") rather than "proto".
func WithCodec(name string, c codec.Codec) Option {
	return &codecOption{
		Name:  name,
		Codec: c,
	}
}

func (o *codecOption) applyToClient(config *clientConfiguration) {
	if o.Name == "" || o.Codec == nil {
		return
	}
	config.Codec = o.Codec
	config.CodecName = o.Name
}

func (o *codecOption) applyToHandler(config *handlerConfiguration) {
	if o.Name == "" {
		return
	}
	if o.Codec == nil {
		delete(config.Codecs, o.Name)
		return
	}
	config.Codecs[o.Name] = o.Codec
}

type compressorOption struct {
	Name       string
	Compressor Compressor
}

// WithCompressor configures client and server compression strategies.
// Registering a compressor with an empty name is a no-op.
//
// For handlers, it registers a compression algorithm. Clients may send
// messages compressed with that algorithm and/or request compressed responses.
//
// For clients, registering compressors serves two purposes. First, the client
// asks servers to compress responses using any of the registered algorithms.
// (gRPC's compression negotiation is complex, but most of Google's gRPC server
// implementations won't compress responses unless the request is compressed.)
// Second, it makes all the registered algorithms available for use with
// WithRequestCompressor. Note that actually compressing requests requires
// using both WithCompressor and WithRequestCompressor.
//
// To remove a previously-registered compressor, re-register the same name with
// a nil compressor.
func WithCompressor(name string, c Compressor) Option {
	return &compressorOption{
		Name:       name,
		Compressor: c,
	}
}

// WithGzip registers a gzip compressor. The compressor uses the standard
// library's gzip package with the default compression level, and it doesn't
// compress messages smaller than 1kb.
//
// Handlers with this option applied accept gzipped requests and can send
// gzipped responses. Clients with this option applied request gzipped
// responses, but don't automatically send gzipped requests (since the server
// may not support them). Use WithGzipRequests to gzip requests.
//
// Handlers and clients generated by protoc-gen-go-connect apply WithGzip by
// default.
func WithGzip() Option {
	return WithCompressor(compressGzip, newGzipCompressor())
}

func (o *compressorOption) applyToClient(config *clientConfiguration) {
	o.apply(config.Compressors)
}

func (o *compressorOption) applyToHandler(config *handlerConfiguration) {
	o.apply(config.Compressors)
}

func (o *compressorOption) apply(m map[string]Compressor) {
	if o.Name == "" {
		return
	}
	if o.Compressor == nil {
		delete(m, o.Name)
		return
	}
	m[o.Name] = o.Compressor
}

type interceptOption struct {
	interceptors []Interceptor
}

// WithInterceptors configures a client or handler's interceptor stack. Repeated
// WithInterceptors options are applied in order, so
//
//   WithInterceptors(A) + WithInterceptors(B, C) == WithInterceptors(A, B, C)
//
// Unary interceptors compose like an onion. The first interceptor provided is
// the outermost layer of the onion: it acts first on the context and request,
// and last on the response and error.
//
// Stream interceptors also behave like an onion: the first interceptor
// provided is the first to wrap the context and is the outermost wrapper for
// the (Sender, Receiver) pair. It's the first to see sent messages and the
// last to see received messages.
//
// Applied to client and handler, WithInterceptors(A, B, ..., Y, Z) produces:
//
//        client.Send()     client.Receive()
//              |                 ^
//              v                 |
//           A ---               --- A
//           B ---               --- B
//             ...               ...
//           Y ---               --- Y
//           Z ---               --- Z
//              |                 ^
//              v                 |
//           network            network
//              |                 ^
//              v                 |
//           A ---               --- A
//           B ---               --- B
//             ...               ...
//           Y ---               --- Y
//           Z ---               --- Z
//              |                 ^
//              v                 |
//       handler.Receive() handler.Send()
//              |                 ^
//              |                 |
//              -> handler logic --
//
// Note that in clients, the Sender handles the request message(s) and the
// Receiver handles the response message(s). For handlers, it's the reverse.
// Depending on your interceptor's logic, you may need to wrap one side of the
// stream on the clients and the other side on handlers.
func WithInterceptors(interceptors ...Interceptor) Option {
	return &interceptOption{interceptors}
}

func (o *interceptOption) applyToClient(config *clientConfiguration) {
	config.Interceptor = o.chainWith(config.Interceptor)
}

func (o *interceptOption) applyToHandler(config *handlerConfiguration) {
	config.Interceptor = o.chainWith(config.Interceptor)
}

func (o *interceptOption) chainWith(current Interceptor) Interceptor {
	if len(o.interceptors) == 0 {
		return current
	}
	if current == nil && len(o.interceptors) == 1 {
		return o.interceptors[0]
	}
	if current == nil && len(o.interceptors) > 1 {
		return newChain(o.interceptors)
	}
	return newChain(append([]Interceptor{current}, o.interceptors...))
}
