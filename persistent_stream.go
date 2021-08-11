package p2pd

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"

	ggio "github.com/gogo/protobuf/io"
	"github.com/gogo/protobuf/proto"
	"github.com/libp2p/go-libp2p-core/protocol"
	pb "github.com/libp2p/go-libp2p-daemon/pb"
)

func (d *Daemon) handleUpgradedConn(r ggio.Reader, unsafeW ggio.Writer) {
	var streamHandlers []string
	defer func() {
		d.mx.Lock()
		defer d.mx.Unlock()

		for _, proto := range streamHandlers {
			p := protocol.ID(proto)
			d.host.RemoveStreamHandler(p)
			d.registeredUnaryProtocols[p] = false
		}
	}()

	if d.cancelTerminateTimer != nil {
		d.cancelTerminateTimer()
	}

	d.terminateWG.Add(1)
	defer d.terminateWG.Done()

	d.terminateOnce.Do(func() { go d.awaitTermination() })

	w := &safeWriter{w: unsafeW}
	for {
		var req pb.PersistentConnectionRequest
		if err := r.ReadMsg(&req); err != nil {
			log.Debugw("error reading message", "error", err)
			return
		}

		callID, err := uuid.FromBytes(req.CallId)
		if err != nil {
			log.Debugw("bad call id: ", "error", err)
			continue
		}

		switch req.Message.(type) {
		case *pb.PersistentConnectionRequest_AddUnaryHandler:
			go func() {
				resp := d.doAddUnaryHandler(w, callID, req.GetAddUnaryHandler())

				d.mx.Lock()
				if _, ok := resp.Message.(*pb.PersistentConnectionResponse_DaemonError); !ok {
					streamHandlers = append(
						streamHandlers,
						*req.GetAddUnaryHandler().Proto,
					)
				}
				d.mx.Unlock()

				if err := w.WriteMsg(resp); err != nil {
					log.Debugw("error reading message", "error", err)
					return
				}
			}()

		case *pb.PersistentConnectionRequest_CallUnary:
			go func() {
				ctx, cancel := context.WithCancel(context.Background())
				d.cancelUnary.Store(callID, cancel)
				defer cancel()

				defer d.cancelUnary.Delete(callID)

				resp := d.doUnaryCall(ctx, callID, &req)

				if err := w.WriteMsg(resp); err != nil {
					log.Debugw("error reading message", "error", err)
					return
				}
			}()

		case *pb.PersistentConnectionRequest_UnaryResponse:
			go func() {
				resp := d.doSendReponseToRemote(&req)
				if err := w.WriteMsg(resp); err != nil {
					log.Debugw("error reading message", "error", err)
					return
				}
			}()

		case *pb.PersistentConnectionRequest_Cancel:
			go func() {
				cf, found := d.cancelUnary.Load(callID)
				if !found {
					return
				}

				cf.(context.CancelFunc)()
			}()
		}
	}
}

func (d *Daemon) doAddUnaryHandler(w ggio.Writer, callID uuid.UUID, req *pb.AddUnaryHandlerRequest) *pb.PersistentConnectionResponse {
	d.mx.Lock()
	defer d.mx.Unlock()

	p := protocol.ID(*req.Proto)
	if registered, found := d.registeredUnaryProtocols[p]; found && registered {
		return errorUnaryCallString(
			callID,
			fmt.Sprintf("handler for protocol %s already set", *req.Proto),
		)
	}

	d.host.SetStreamHandler(p, d.getPersistentStreamHandler(w))
	d.registeredUnaryProtocols[p] = true

	log.Debugw("set unary stream handler", "protocol", p)

	return okUnaryCallResponse(callID)
}

func (d *Daemon) doUnaryCall(ctx context.Context, callID uuid.UUID, req *pb.PersistentConnectionRequest) *pb.PersistentConnectionResponse {
	pid, err := peer.IDFromBytes(req.GetCallUnary().Peer)
	if err != nil {
		return errorUnaryCall(callID, err)
	}

	remoteStream, err := d.host.NewStream(
		ctx,
		pid,
		protocol.ID(*req.GetCallUnary().Proto),
	)
	if err != nil {
		return errorUnaryCall(callID, err)
	}
	defer remoteStream.Close()

	select {
	case response := <-exchangeMessages(ctx, remoteStream, req):
		return response

	case <-ctx.Done():
		return okCancelled(callID)
	}
}

func exchangeMessages(ctx context.Context, s network.Stream, req *pb.PersistentConnectionRequest) <-chan *pb.PersistentConnectionResponse {
	callID, _ := uuid.FromBytes(req.CallId)
	rc := make(chan *pb.PersistentConnectionResponse)

	go func() {
		defer close(rc)

		if err := ggio.NewDelimitedWriter(s).WriteMsg(req); ctx.Err() != nil {
			return
		} else if err != nil {
			rc <- errorUnaryCall(callID, err)
			return
		}

		remoteResp := &pb.PersistentConnectionRequest{}
		if err := ggio.NewDelimitedReader(s, network.MessageSizeMax).ReadMsg(remoteResp); ctx.Err() != nil {
			return
		} else if err != nil {
			rc <- errorUnaryCall(callID, err)
			return
		}

		resp := okUnaryCallResponse(callID)
		resp.Message = &pb.PersistentConnectionResponse_CallUnaryResponse{
			CallUnaryResponse: remoteResp.GetUnaryResponse(),
		}

		select {
		case rc <- resp:
			return

		case <-ctx.Done():
			return
		}
	}()

	return rc
}

// awaitReadFail writers to a semaphor channel if the given io.Reader fails to
// read before the context was cancelled
func awaitReadFail(ctx context.Context, r io.Reader) <-chan struct{} {
	semaphor := make(chan struct{})

	go func() {
		defer close(semaphor)

		buff := make([]byte, 1)
		if _, err := r.Read(buff); err != nil {
			select {
			case semaphor <- struct{}{}:
			case <-ctx.Done():
			}
		}
	}()

	return semaphor
}

// getPersistentStreamHandler returns a lib-p2p stream handler tied to a
// given persistent client stream
func (d *Daemon) getPersistentStreamHandler(cw ggio.Writer) network.StreamHandler {
	return func(s network.Stream) {
		defer s.Close()

		req := &pb.PersistentConnectionRequest{}
		if err := ggio.NewDelimitedReader(s, network.MessageSizeMax).ReadMsg(req); err != nil {
			log.Debugw("failed to read proto from incoming p2p stream", "error", err)
			return
		}

		// now the peer field stores the caller's peer id
		req.GetCallUnary().Peer = []byte(s.Conn().RemotePeer())

		callID, err := uuid.FromBytes(req.CallId)
		if err != nil {
			log.Debugw("bad call id in p2p handler", "error", err)
			return
		}

		rc := make(chan *pb.PersistentConnectionRequest)
		d.responseWaiters.Store(callID, rc)
		defer d.responseWaiters.Delete(callID)

		ctx, cancel := context.WithCancel(d.ctx)
		defer cancel()

		resp := &pb.PersistentConnectionResponse{
			CallId: req.CallId,
			Message: &pb.PersistentConnectionResponse_RequestHandling{
				RequestHandling: req.GetCallUnary(),
			},
		}
		if err := cw.WriteMsg(resp); err != nil {
			log.Debugw("failed to write message to client", "error", err)
			return
		}

		select {
		case <-awaitReadFail(ctx, s):
			if err := cw.WriteMsg(
				&pb.PersistentConnectionResponse{
					CallId: callID[:],
					Message: &pb.PersistentConnectionResponse_Cancel{
						Cancel: &pb.Cancel{},
					},
				},
			); err != nil {
				log.Debugw("failed to write to client", "error", err)
				return
			}
			return
		case response := <-rc:
			w := ggio.NewDelimitedWriter(s)
			if err := w.WriteMsg(response); err != nil {
				log.Debugw("failed to write message to remote", "error", err)
			}
		}
	}
}

func (d *Daemon) doSendReponseToRemote(req *pb.PersistentConnectionRequest) *pb.PersistentConnectionResponse {
	callID, err := uuid.FromBytes(req.CallId)
	if err != nil {
		return errorUnaryCallString(
			callID,
			"malformed request: call id not in UUID format",
		)
	}

	rc, found := d.responseWaiters.Load(callID)
	if !found {
		return errorUnaryCallString(
			callID,
			fmt.Sprintf("Response for call id %d not requested or cancelled", callID),
		)
	}

	rc.(chan *pb.PersistentConnectionRequest) <- req

	return okUnaryCallResponse(callID)
}

type safeWriter struct {
	w ggio.Writer
	m sync.Mutex
}

func (sw *safeWriter) WriteMsg(msg proto.Message) error {
	sw.m.Lock()
	defer sw.m.Unlock()
	return sw.w.WriteMsg(msg)
}

func errorUnaryCall(callID uuid.UUID, err error) *pb.PersistentConnectionResponse {
	message := err.Error()
	return &pb.PersistentConnectionResponse{
		CallId: callID[:],
		Message: &pb.PersistentConnectionResponse_DaemonError{
			DaemonError: &pb.DaemonError{Message: &message},
		},
	}
}

func errorUnaryCallString(callID uuid.UUID, errMsg string) *pb.PersistentConnectionResponse {
	return &pb.PersistentConnectionResponse{
		CallId: callID[:],
		Message: &pb.PersistentConnectionResponse_DaemonError{
			DaemonError: &pb.DaemonError{Message: &errMsg},
		},
	}
}

func okUnaryCallResponse(callID uuid.UUID) *pb.PersistentConnectionResponse {
	return &pb.PersistentConnectionResponse{CallId: callID[:]}
}

func okCancelled(callID uuid.UUID) *pb.PersistentConnectionResponse {
	return &pb.PersistentConnectionResponse{
		CallId: callID[:],
		Message: &pb.PersistentConnectionResponse_Cancel{
			Cancel: &pb.Cancel{},
		},
	}
}
