package main

type broadcastService struct {
	broadcast chan []byte
	listeners []chan []byte
	addChan chan (chan []byte)
	delChan chan (chan []byte)
}

func newBroadcastService() *broadcastService {
	return &broadcastService{
		broadcast: make(chan []byte),
		listeners: make([]chan []byte, 3),
		addChan:   make(chan (chan []byte)),
		delChan:   make(chan (chan []byte)),
	}
}

func (bs *broadcastService) add() chan []byte {
	ch := make(chan []byte)
	bs.addChan <- ch
	return ch
}

func (bs *broadcastService) del(ch chan []byte) {
	bs.delChan <- ch
}

func (bs *broadcastService) run() {
	Loop: for {
		select {
		case ch := <- bs.addChan:
			for i, v := range bs.listeners {
				if v == nil {
					bs.listeners[i] = ch
					continue Loop
				}
			}
			bs.listeners = append(bs.listeners, ch)
		case ch := <- bs.delChan:
			for i, v := range bs.listeners {
				if v == ch {
					bs.listeners[i] = nil
					close(ch)
					continue Loop
				}
			}
		case v, ok := <- bs.broadcast:
			if !ok {
				for _, ch := range bs.listeners {
					if ch != nil {
						close(ch)
					}
				}
				return
			}

			for _, ch := range bs.listeners {
				if ch != nil {
					ch <- v
				}
			}
		}
	}
}
