// Command flood opens connections, sends an unauthenticated PUT with part of
// a large body, reads the refusal and holds the socket for a moment: what a
// client does that keeps sending while it is refused. What it sent stays in
// the server's socket buffers, which count against the container's memory.
// test/load/flood.sh runs it.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	addr := flag.String("addr", "", "")
	path := flag.String("path", "/repository/raw-hosted/flood", "")
	workers := flag.Int("workers", 1400, "")
	seconds := flag.Int("seconds", 12, "")
	send := flag.Int("send", 512<<10, "bytes of body to try to send")
	hold := flag.Duration("hold", 700*time.Millisecond, "")
	flag.Parse()
	chunk := make([]byte, *send)
	end := time.Now().Add(time.Duration(*seconds) * time.Second)
	var n, refused, errs atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; time.Now().Before(end); i++ {
				c, err := net.DialTimeout("tcp", *addr, 5*time.Second)
				if err != nil {
					errs.Add(1)
					time.Sleep(50 * time.Millisecond)
					continue
				}
				n.Add(1)
				c.SetDeadline(time.Now().Add(3 * time.Second))
				fmt.Fprintf(c, "PUT %s/%d-%d.bin HTTP/1.1\r\nHost: x\r\nContent-Length: 5242880\r\n\r\n", *path, w, i)
				go func() { c.Write(chunk) }() // as much as the socket takes
				if line, err := bufio.NewReader(c).ReadString('\n'); err == nil && len(line) > 9 && line[9] == '4' {
					refused.Add(1)
				}
				time.Sleep(*hold)
				c.(*net.TCPConn).SetLinger(0)
				c.Close()
			}
		}(w)
	}
	wg.Wait()
	fmt.Printf("connections %d, refused %d, dial errors %d, %.0f/s\n", n.Load(), refused.Load(), errs.Load(), float64(n.Load())/float64(*seconds))
}
