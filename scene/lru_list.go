package scene

// listElement is a node in the intrusive LRU doubly-linked list.
type listElement struct {
	prev, next *listElement
	Value      *layerEntry
}

// lruList is a minimal doubly-linked list ordered most-recently-used first.
// It is not safe for concurrent access on its own; LayerPixelCache holds its
// mutex around every operation.
type lruList struct {
	front *listElement
	back  *listElement
	len   int
}

// Init empties the list.
func (l *lruList) Init() {
	l.front, l.back, l.len = nil, nil, 0
}

// Len returns the number of elements.
func (l *lruList) Len() int { return l.len }

// PushFront inserts an element carrying entry and returns it.
func (l *lruList) PushFront(entry *layerEntry) *listElement {
	e := &listElement{Value: entry}
	if l.len == 0 {
		l.front, l.back = e, e
	} else {
		e.next = l.front
		l.front.prev = e
		l.front = e
	}
	l.len++
	return e
}

// Remove unlinks e from the list.
func (l *lruList) Remove(e *listElement) {
	if e == nil {
		return
	}
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		l.front = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		l.back = e.prev
	}
	e.prev, e.next = nil, nil
	l.len--
}

// MoveToFront moves an existing element to the most-recently-used position.
func (l *lruList) MoveToFront(e *listElement) {
	if e == nil || l.front == e {
		return
	}
	l.Remove(e)
	e.prev, e.next = nil, nil
	if l.len == 0 {
		l.front, l.back = e, e
		l.len = 1
		return
	}
	e.next = l.front
	l.front.prev = e
	l.front = e
	l.len++
}

// Front returns the most-recently-used element.
func (l *lruList) Front() *listElement { return l.front }

// Back returns the least-recently-used element.
func (l *lruList) Back() *listElement { return l.back }
