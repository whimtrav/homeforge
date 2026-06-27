package bus

import "sync"

type Event struct {
	Topic   string
	Payload any
}

type Handler func(Event)

type Bus struct {
	mu       sync.RWMutex
	handlers map[string][]Handler
}

func New() *Bus {
	return &Bus{handlers: make(map[string][]Handler)}
}

func (b *Bus) Subscribe(topic string, h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[topic] = append(b.handlers[topic], h)
}

func (b *Bus) Publish(topic string, payload any) {
	b.mu.RLock()
	handlers := append([]Handler(nil), b.handlers[topic]...)
	wildcards := append([]Handler(nil), b.handlers["*"]...)
	b.mu.RUnlock()

	ev := Event{Topic: topic, Payload: payload}
	for _, h := range append(handlers, wildcards...) {
		h(ev)
	}
}
