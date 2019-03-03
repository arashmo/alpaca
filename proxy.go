package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

type proxyHandler struct {
	pacRunner *PacRunner
}

func NewProxyHandler(pacUrl string) (http.Handler, error) {
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Get(pacUrl)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return newProxyHandler(resp.Body)
}

func NewHardCodedProxyHandler(proxy string) (http.Handler, error) {
	return newProxyHandler(strings.NewReader(fmt.Sprintf(
		`function FindProxyForURL(url, host) { return "%s" }`, proxy)))
}

func newProxyHandler(r io.Reader) (http.Handler, error) {
	pr, err := NewPacRunner(r)
	if err != nil {
		return nil, err
	}
	return proxyHandler{pr}, nil
}

func (ph proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fmt.Println("dumping request:")
	r.Write(os.Stdout)
	proxy, err := ph.pacRunner.FindProxyForURL(r.URL)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if r.Method == http.MethodConnect {
		if proxy == "DIRECT" {
			connect(w, r)
		} else {
			tunnel(w, r, proxy)
		}
	} else {
		direct(w, r, proxy)
	}
}

func tunnel(w http.ResponseWriter, r *http.Request, proxy string) {
	// can't hijack the connection to server, so can't just replay request
	// need to dial and manually write connect header and read response
	proxyUrl, err := url.Parse(proxy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	proxyConn, err := net.Dial("tcp", proxyUrl.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r.Write(proxyConn)
	pr := bufio.NewReader(proxyConn)
	res, err := http.ReadResponse(pr, r)
	// should we close the response body, or leave it so that the
	// connection stays open?
	// ...also, might need to check for any buffered data in the reader,
	// and write it to the connection before moving on
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h, ok := w.(http.Hijacker)
	if !ok {
		msg := fmt.Sprintf("Can't hijack connection to %v", r.Host)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(res.StatusCode)
	if res.StatusCode != http.StatusOK {
		return
	}
	client, _, err := h.Hijack()
	if err != nil {
		// The response status has already been sent, so if hijacking
		// fails, we can't return an error status to the client.
		// Instead, log the error and finish up.
		log.Printf("Error hijacking connection to %v", r.Host)
		proxyConn.Close()
		return
	}
	go func() {
		defer proxyConn.Close()
		defer client.Close()
		var wg sync.WaitGroup
		wg.Add(2)
		go transfer(&wg, proxyConn, client)
		go transfer(&wg, client, proxyConn)
		wg.Wait()
	}()
}

func connect(w http.ResponseWriter, r *http.Request) {
	// TODO: should probably put a timeout on this
	server, err := net.Dial("tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h, ok := w.(http.Hijacker)
	if !ok {
		msg := fmt.Sprintf("Can't hijack connection to %v", r.Host)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	client, _, err := h.Hijack()
	if err != nil {
		// The response status has already been sent, so if hijacking
		// fails, we can't return an error status to the client.
		// Instead, log the error and finish up.
		log.Printf("Error hijacking connection to %v", r.Host)
		server.Close()
		return
	}
	go func() {
		defer server.Close()
		defer client.Close()
		var wg sync.WaitGroup
		wg.Add(2)
		go transfer(&wg, server, client)
		go transfer(&wg, client, server)
		wg.Wait()
	}()
}

func transfer(wg *sync.WaitGroup, dst, src net.Conn) {
	defer wg.Done()
	_, err := io.Copy(dst, src)
	if err != nil {
		log.Printf("Error copying from %v to %v",
			src.RemoteAddr().String(), dst.RemoteAddr().String())
	}
}

func direct(w http.ResponseWriter, r *http.Request, proxy string) {
	log.Printf("direct: r.url = %s\n", r.URL.String())
	// TODO: reuse the Transport, don't build a new one each time
	proxyFunc := func(r *http.Request) (*url.URL, error) {
		if proxy == "DIRECT" {
			return nil, nil
		}
		return url.Parse(proxy)
	}
	t := &http.Transport{Proxy: proxyFunc}
	resp, err := t.RoundTrip(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	// TODO: Don't retransmit hop-by-hop headers.
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers#hbh
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		// The response status has already been sent, so if copying
		// fails, we can't return an error status to the client.
		// Instead, log the error.
		log.Printf("Error copying response body from %v", r.Host)
		return
	}
}