package queue

import (
	"bytes"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/StackExchange/scollector/collectors"
	"github.com/StackExchange/scollector/opentsdb"
	"github.com/StackExchange/slog"
	"github.com/mreiferson/go-httpclient"
)

var l = log.New(os.Stdout, "", log.LstdFlags)

type Queue struct {
	sync.Mutex
	host  string
	queue opentsdb.MultiDataPoint
	c     chan *opentsdb.DataPoint
}

// Creates and starts a new Queue.
func New(host string, c chan *opentsdb.DataPoint) *Queue {
	const _100MB = 1024 * 1024 * 100
	q := Queue{
		host: host,
		c:    c,
	}
	var m runtime.MemStats
	go func() {
		for _ = range time.Tick(time.Minute) {
			runtime.ReadMemStats(&m)
			if m.Alloc > _100MB {
				runtime.GC()
			}
		}
	}()
	go func() {
		for dp := range c {
			if m.Alloc > _100MB {
				collectors.IncScollector("dropped", 1)
				continue
			}
			q.Lock()
			q.queue = append(q.queue, dp)
			q.Unlock()
		}
	}()
	go q.send()
	return &q
}

var BatchSize = 50

func (q *Queue) send() {
	for {
		if len(q.queue) > 0 {
			q.Lock()
			i := len(q.queue)
			if i > BatchSize {
				i = BatchSize
			}
			sending := q.queue[:i]
			q.queue = q.queue[i:]
			q.Unlock()
			slog.Infof("sending: %d, remaining: %d", len(sending), len(q.queue))
			q.sendBatch(sending)
		} else {
			time.Sleep(time.Second)
		}
	}
}

var qlock sync.Mutex
var client = &http.Client{
	Transport: &httpclient.Transport{
		RequestTimeout: time.Minute,
	},
}

func (q *Queue) sendBatch(batch opentsdb.MultiDataPoint) {
	b, err := batch.Json()
	if err != nil {
		slog.Error(err)
		// bad JSON encoding, just give up
		return
	}
	resp, err := client.Post(q.host, "application/json", bytes.NewReader(b))
	if resp != nil && resp.Body != nil {
		defer func() { resp.Body.Close() }()
	}
	// Some problem with connecting to the server; retry later.
	if err != nil || resp.StatusCode != http.StatusNoContent {
		if err != nil {
			slog.Error(err)
		} else if resp.StatusCode != http.StatusNoContent {
			slog.Errorln(resp.Status)
			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				slog.Error(err)
			}
			if len(body) > 0 {
				slog.Error(string(body))
			}
		}
		t := time.Now().Add(-time.Minute * 30).Unix()
		old := 0
		restored := 0
		for _, dp := range batch {
			if dp.Timestamp < t {
				old++
				continue
			}
			restored++
			q.c <- dp
		}
		if old > 0 {
			slog.Infof("removed %d old records", old)
		}
		d := time.Second * 5
		slog.Infof("restored %d, sleeping %s", restored, d)
		time.Sleep(d)
		return
	} else {
		slog.Infoln("sent", len(batch))
		collectors.IncScollector("sent", len(batch))
	}
}
