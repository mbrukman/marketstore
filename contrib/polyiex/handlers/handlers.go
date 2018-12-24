package handlers

import (
	"fmt"
	"sync"
	"time"

	"github.com/alpacahq/marketstore/contrib/polyiex/orderbook"
	"github.com/alpacahq/marketstore/utils/io"
	"github.com/alpacahq/marketstore/utils/log"
	"github.com/buger/jsonparser"
	"github.com/eapache/channels"
)

func TradeEach(raw []byte) {
	symbol, err := jsonparser.GetString(raw, "S")
	if err != nil {
		log.Error("[polyiex] unexpected message: %v", string(raw))
		return
	}

	price, _ := jsonparser.GetFloat(raw, "p")
	size, _ := jsonparser.GetInt(raw, "s")
	millisec, _ := jsonparser.GetInt(raw, "t")
	nanosec, _ := jsonparser.GetInt(raw, "T")

	if price <= 0.0 || size <= 0 {
		// ignore
		return
	}

	timestamp := time.Unix(0, 1000*1000*millisec+nanosec)

	pkt := &writePacket{
		io.NewTimeBucketKey(symbol + "/1Min/TRADE"),
		&trade{
			epoch: timestamp.Unix(),
			nanos: int32(timestamp.Nanosecond()),
			px:    float32(price),
			sz:    int32(size),
		}}
	Write(pkt)
}

func Trade(raw []byte) {
	jsonparser.ArrayEach(raw, func(value []byte, dataType jsonparser.ValueType, offset int, err error) {
		TradeEach(value)
	})
}

func BookEach(raw []byte) {
	symbol, err := jsonparser.GetString(raw, "S")
	if err != nil {
		log.Error("[polyiex] unexpected message: %v", string(raw))
		return
	}
	millisec, _ := jsonparser.GetInt(raw, "t")
	nanosec, _ := jsonparser.GetInt(raw, "T")

	book := getOrderBook(symbol)
	jsonparser.ArrayEach(raw, func(value []byte, dataType jsonparser.ValueType, offset int, err error) {
		px, _ := jsonparser.GetFloat(value, "[0]")
		sz, _ := jsonparser.GetInt(value, "[1]")
		book.Bid(orderbook.Entry{Price: float32(px), Size: int32(sz)})
	}, "b")
	jsonparser.ArrayEach(raw, func(value []byte, dataType jsonparser.ValueType, offset int, err error) {
		px, _ := jsonparser.GetFloat(value, "[0]")
		sz, _ := jsonparser.GetInt(value, "[1]")
		book.Ask(orderbook.Entry{Price: float32(px), Size: int32(sz)})
	}, "a")

	b, a := book.BBO()

	//if symbol == "SPY" {
	print(string(raw))
	fmt.Printf("[polyiex] BBO[%s]=(%v)/(%v)\n", symbol, b, a)
	//}

	// maybe we should skip to write if BBO isn't changed
	timestamp := time.Unix(0, 1000*1000*millisec+nanosec)
	pkt := &writePacket{
		io.NewTimeBucketKey(symbol + "/1Min/QUOTE"),
		&quote{
			epoch: timestamp.Unix(),
			nanos: int32(timestamp.Nanosecond()),
			bidPx: b.Price,
			askPx: a.Price,
			bidSz: b.Size,
			askSz: a.Size,
		}}

	Write(pkt)
}

func Book(raw []byte) {
	// log.Info("ID: %v", string(raw))

	jsonparser.ArrayEach(raw, func(value []byte, dataType jsonparser.ValueType, offset int, err error) {
		BookEach(value)
	})
}

// orderBooks is a map of OrderBook with symbol key
var orderBooks = map[string]*orderbook.OrderBook{}
var obMutex sync.Mutex

func getOrderBook(symbol string) *orderbook.OrderBook {
	obMutex.Lock()
	defer obMutex.Unlock()
	book, ok := orderBooks[symbol]
	if !ok {
		book = orderbook.NewOrderBook()
		orderBooks[symbol] = book
	}
	return book
}

type trade struct {
	epoch int64
	nanos int32
	px    float32
	sz    int32
}

type quote struct {
	epoch int64   // 8
	nanos int32   // 4
	bidPx float32 // 4
	askPx float32 // 4
	bidSz int32   // 4
	askSz int32   // 4
}

var (
	w = &writer{
		dataBuckets: map[io.TimeBucketKey]interface{}{},
		interval:    100 * time.Millisecond,
		c:           channels.NewInfiniteChannel(),
	}
	once sync.Once
)

type writePacket struct {
	tbk  *io.TimeBucketKey
	data interface{}
}

type writer struct {
	sync.Mutex
	dataBuckets map[io.TimeBucketKey]interface{}
	interval    time.Duration
	c           *channels.InfiniteChannel
}

func (w *writer) write() {
	// preallocate the data structures for re-use
	var (
		csm io.ColumnSeriesMap

		epoch []int64
		nanos []int32
		bidPx []float32
		askPx []float32
		px    []float32
		bidSz []int32
		askSz []int32
		sz    []int32
	)

	for {
		select {
		case m := <-w.c.Out():
			w.Lock()
			packet := m.(*writePacket)

			if bucket, ok := w.dataBuckets[*packet.tbk]; ok {
				switch packet.data.(type) {
				case *quote:
					w.dataBuckets[*packet.tbk] = append(bucket.([]*quote), packet.data.(*quote))
				case *trade:
					w.dataBuckets[*packet.tbk] = append(bucket.([]*trade), packet.data.(*trade))
				}
			} else {
				switch packet.data.(type) {
				case *quote:
					w.dataBuckets[*packet.tbk] = []*quote{packet.data.(*quote)}
				case *trade:
					w.dataBuckets[*packet.tbk] = []*trade{packet.data.(*trade)}
				}
			}

			w.Unlock()

		case <-time.After(w.interval):
			w.Lock()
			csm = io.NewColumnSeriesMap()

			for tbk, bucket := range w.dataBuckets {
				switch bucket.(type) {
				case []*quote:
					b := bucket.([]*quote)

					for _, q := range b {
						epoch = append(epoch, q.epoch)
						nanos = append(nanos, q.nanos)
						bidPx = append(bidPx, q.bidPx)
						askPx = append(askPx, q.askPx)
						bidSz = append(bidSz, q.bidSz)
						askSz = append(askSz, q.askSz)
					}

					if len(epoch) > 0 {
						csm.AddColumn(tbk, "Epoch", epoch)
						csm.AddColumn(tbk, "Nanoseconds", nanos)
						csm.AddColumn(tbk, "BidPrice", bidPx)
						csm.AddColumn(tbk, "AskPrice", askPx)
						csm.AddColumn(tbk, "BidSize", bidSz)
						csm.AddColumn(tbk, "AskSize", askSz)

						// trim the slices
						epoch = epoch[:0]
						nanos = nanos[:0]
						bidPx = bidPx[:0]
						bidSz = bidSz[:0]
						askPx = bidPx[:0]
						askSz = askSz[:0]
						w.dataBuckets[tbk] = b[:0]
					}
				case []*trade:
					b := bucket.([]*trade)

					for _, t := range b {
						epoch = append(epoch, t.epoch)
						nanos = append(nanos, t.nanos)
						px = append(px, t.px)
						sz = append(sz, t.sz)
					}

					if len(epoch) > 0 {
						csm.AddColumn(tbk, "Epoch", epoch)
						csm.AddColumn(tbk, "Nanoseconds", nanos)
						csm.AddColumn(tbk, "Price", px)
						csm.AddColumn(tbk, "Size", sz)

						// trim the slices
						epoch = epoch[:0]
						nanos = nanos[:0]
						px = px[:0]
						sz = sz[:0]
						w.dataBuckets[tbk] = b[:0]
					}
				}
			}

			w.Unlock()

			// if err := executor.WriteCSM(csm, true); err != nil {
			// 	log.Error("[polygon] failed to write csm (%v)", err)
			// }
		}
	}
}

func Write(pkt *writePacket) {
	once.Do(func() {
		go w.write()
	})

	w.c.In() <- pkt
}