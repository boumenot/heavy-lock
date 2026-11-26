package transport

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	heavylockv2 "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const DefaultPort = 49783

type Broker interface {
	heavylockv2.ControlServiceServer
	heavylockv2.RunnerServiceServer
}

var ErrPortInUse = errors.New("broker loopback port is already in use")

type Server struct {
	listener net.Listener
	http     *http.Server
	grpc     *grpc.Server
	port     int
}

func Listen(port int, broker Broker) (*Server, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("broker port must be between 1 and 65535: %d", port)
	}
	if broker == nil {
		return nil, errors.New("broker is required")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("%w on 127.0.0.1:%d: %w", ErrPortInUse, port, err)
		}
		return nil, fmt.Errorf("listen on broker loopback port %d: %w", port, err)
	}
	return newServer(listener, broker), nil
}

func newServer(listener net.Listener, broker Broker) *Server {
	port := listener.Addr().(*net.TCPAddr).Port
	handler := newHandler(broker, port)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	return &Server{
		listener: listener,
		http: &http.Server{
			Handler:           handler,
			Protocols:         protocols,
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
		grpc: handler.grpc,
		port: port,
	}
}

func (s *Server) Port() int {
	return s.port
}

func (s *Server) Serve() error {
	err := s.http.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Close() error {
	httpErr := s.http.Close()
	s.grpc.Stop()
	listenerErr := s.listener.Close()
	if errors.Is(listenerErr, net.ErrClosed) {
		listenerErr = nil
	}
	return errors.Join(httpErr, listenerErr)
}

type handler struct {
	grpc *grpc.Server
	rest http.Handler
	port int
}

func newHandler(broker Broker, port int) *handler {
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
			response, err := next(ctx, req)
			return response, contract.NormalizeError(err)
		}),
		grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) error {
			return contract.NormalizeError(next(srv, stream))
		}),
		grpc.UnknownServiceHandler(func(_ any, _ grpc.ServerStream) error {
			return contract.Error(codes.Unimplemented, heavylockv2.ErrorReason_OPERATION_NOT_IMPLEMENTED, "unknown gRPC method", nil)
		}),
	)
	heavylockv2.RegisterControlServiceServer(grpcServer, broker)
	heavylockv2.RegisterRunnerServiceServer(grpcServer, broker)
	return &handler{grpc: grpcServer, rest: newRESTHandler(broker), port: port}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	isGRPC := mediaType == "application/grpc" || strings.HasPrefix(mediaType, "application/grpc+")
	if !isGRPC {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	if !validAuthority(r.Host, h.port) {
		err := invalidField("host", "Host or authority must name this loopback listener")
		if isGRPC && r.ProtoMajor == 2 {
			writeGRPCError(w, err)
		} else {
			writeRESTError(w, err, http.StatusBadRequest)
		}
		return
	}
	if isGRPC {
		if r.ProtoMajor != 2 {
			writeRESTError(w, invalidField("protocol", "gRPC requires HTTP/2"), http.StatusBadRequest)
			return
		}
		h.grpc.ServeHTTP(w, r)
		return
	}
	h.rest.ServeHTTP(w, r)
}

func validAuthority(authority string, port int) bool {
	host := authority
	if strings.Contains(authority, ":") {
		var portText string
		var err error
		host, portText, err = net.SplitHostPort(authority)
		if err != nil {
			return false
		}
		gotPort, err := strconv.Atoi(portText)
		if err != nil || gotPort != port {
			return false
		}
	}
	return host == "127.0.0.1" || strings.EqualFold(host, "localhost")
}

func writeGRPCError(w http.ResponseWriter, err error) {
	result := status.Convert(contract.NormalizeError(err))
	encoded, marshalErr := proto.Marshal(result.Proto())
	if marshalErr != nil {
		http.Error(w, "could not encode gRPC error: "+marshalErr.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("Grpc-Status", strconv.Itoa(int(result.Code())))
	w.Header().Set("Grpc-Message", url.PathEscape(result.Message()))
	w.Header().Set("Grpc-Status-Details-Bin", base64.StdEncoding.EncodeToString(encoded))
	w.WriteHeader(http.StatusOK)
}
