package lb

import (
	"context"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/pkg/errors"
)

type HTTPFrontendRoute struct {
	Host    *regexp.Regexp
	Path    *regexp.Regexp
	Backend *HTTPBackend
}

type HTTPFrontendOptions struct {
	Timeout        time.Duration
	DefaultBackend *HTTPBackend
	Routes         []HTTPFrontendRoute
}

func (o *HTTPFrontendOptions) CopyFrom(src *HTTPFrontendOptions) {
	*o = *src
	o.Routes = make([]HTTPFrontendRoute, len(src.Routes))
	copy(o.Routes, src.Routes)
}

type HTTPFrontend struct {
	opts HTTPFrontendOptions
}

func NewHTTPFrontend(opts HTTPFrontendOptions) (f *HTTPFrontend, err error) {
	f, err = f.Fork(opts)
	return
}

func (f *HTTPFrontend) Fork(opts HTTPFrontendOptions) (fn *HTTPFrontend, err error) {
	fn = &HTTPFrontend{}
	fn.opts.CopyFrom(&opts)
	return
}

func (f *HTTPFrontend) Close() {
}

func (f *HTTPFrontend) GetOpts() (opts HTTPFrontendOptions) {
	opts.CopyFrom(&f.opts)
	return
}

func (f *HTTPFrontend) getBackend(feStatusLine string, feHdr http.Header) (b *HTTPBackend) {
	return f.opts.DefaultBackend
}

func (f *HTTPFrontend) beServe(ctx context.Context, okCh chan<- bool, feConn *bufConn, feStatusLine string, feHdr http.Header, beConn *bufConn) {
	ok := false
	defer func() {
		if !ok {
			beConn.Flush()
			beConn.Close()
		}
		okCh <- ok
	}()

	ingressOKCh := make(chan bool, 1)
	go func() {
		var err error
		defer func() { ingressOKCh <- err == nil }()
		_, err = writeHTTPHeader(beConn.Writer, feStatusLine, feHdr)
		if err != nil {
			debugLogger.Printf("write header to backend %v from frontend %v: %v\n", beConn.RemoteAddr(), feConn.RemoteAddr(), err)
			return
		}
		_, err = writeHTTPBody(beConn.Writer, feConn.Reader, feHdr, true)
		if err != nil {
			if errors.Cause(err) != eofBody {
				debugLogger.Printf("write body to backend %v from frontend %v: %v\n", beConn.RemoteAddr(), feConn.RemoteAddr(), err)
			}
			return
		}
	}()

	beStatusLine, beHdr, _, err := splitHTTPHeader(beConn.Reader)
	if err != nil {
		debugLogger.Printf("read header from backend %v: %v\n", beConn.RemoteAddr(), err)
		return
	}

	_, err = writeHTTPHeader(feConn.Writer, beStatusLine, beHdr)
	if err != nil {
		debugLogger.Printf("write header to frontend %v from backend %v: %v\n", feConn.RemoteAddr(), beConn.RemoteAddr(), err)
		return
	}

	_, err = writeHTTPBody(feConn.Writer, beConn.Reader, beHdr, false)
	if err != nil {
		if errors.Cause(err) != eofBody {
			debugLogger.Printf("write body to frontend %v from backend %v: %v\n", feConn.RemoteAddr(), beConn.RemoteAddr(), err)
		}
		return
	}

	if ingressOK := <-ingressOKCh; !ingressOK {
		return
	}

	switch strings.ToLower(beHdr.Get("Connection")) {
	case "keep-alive":
	case "close":
		fallthrough
	default:
		return
	}

	if beConn.Reader.Buffered() != 0 {
		debugLogger.Printf("buffer order error on backend %v\n", beConn.RemoteAddr())
		return
	}

	ok = true
}

func (f *HTTPFrontend) feServe(ctx context.Context, okCh chan<- bool, feConn *bufConn) {
	ok := false
	defer func() {
		if !ok {
			feConn.Flush()
			feConn.Close()
		}
		okCh <- ok
	}()

	feStatusLine, feHdr, nr, err := splitHTTPHeader(feConn.Reader)
	if err != nil {
		if nr > 0 {
			debugLogger.Printf("read header from frontend %v: %v\n", feConn.RemoteAddr(), err)
		}
		feConn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		return
	}

	b := f.getBackend(feStatusLine, feHdr)
	bs := b.FindServer(ctx)
	if bs == nil {
		feConn.Write([]byte("HTTP/1.1 503 Service Unavailable\r\n\r\n"))
		return
	}

	beConn, err := bs.ConnAcquire(ctx)
	if err != nil {
		feConn.Write([]byte("HTTP/1.1 503 Service Unavailable\r\n\r\n"))
		return
	}
	if tcpConn, ok := beConn.Conn().(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(1 * time.Second)
	}

	feHdr.Set("X-Forwarded-For", feHdr.Get("X-Forwarded-For")+", "+feConn.RemoteAddr().String())

	beCtx, beCtxCancel := ctx, context.CancelFunc(func() {})
	if b.opts.Timeout > 0 {
		beCtx, beCtxCancel = context.WithTimeout(beCtx, b.opts.Timeout)
	}
	for k, v := range b.opts.ReqHeader {
		for ks, vs := range v {
			if ks == 0 {
				feHdr.Set(k, vs)
				continue
			}
			feHdr.Add(k, vs)
		}
	}

	beOK := false
	beOKCh := make(chan bool, 1)
	go f.beServe(beCtx, beOKCh, feConn, feStatusLine, feHdr, beConn)
	select {
	case <-beCtx.Done():
		beConn.Close()
	case beOK = <-beOKCh:
	}
	beCtxCancel()
	if !beOK {
		return
	}

	bs.ConnRelease(beConn)

	if feConn.Reader.Buffered() != 0 {
		debugLogger.Printf("buffer order error on frontend %v\n", feConn.RemoteAddr())
		return
	}

	ok = true
}

func (f *HTTPFrontend) Serve(ctx context.Context, conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(1 * time.Second)
	}
	feConn := newBufConn(conn)
	debugLogger.Printf("connected %v to frontend %v\n", feConn.RemoteAddr(), feConn.LocalAddr())
	for {
		feCtx, feCtxCancel := ctx, context.CancelFunc(func() {})
		if f.opts.Timeout > 0 {
			feCtx, feCtxCancel = context.WithTimeout(feCtx, f.opts.Timeout)
		}

		feOK := false
		feOKCh := make(chan bool, 1)
		go f.feServe(feCtx, feOKCh, feConn)
		select {
		case <-feCtx.Done():
			feConn.Close()
		case feOK = <-feOKCh:
		}
		feCtxCancel()
		if !feOK {
			break
		}
	}
	debugLogger.Printf("disconnected %v to frontend %v\n", feConn.RemoteAddr(), feConn.LocalAddr())
	return
}
