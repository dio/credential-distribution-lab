// Package model is a bounded executable protocol model, not a credential service.
// Time, identity, transport authentication and randomness are abstract inputs.
package model

const Lease = 2

// Binding includes both possession scope and permitted use. Destinations are
// logical identities, not arbitrary URLs accepted from a caller.
type Binding struct {
	Tenant, Backend, Destination, Key, Version int
}

type Config struct {
	Revision int
	Binding  Binding
}

type Credential struct {
	Tenant, Key, Version, Expires int
	Revoked                       bool
}

type Publication struct {
	Epoch, Revision int
	Records         [2]Credential
}

type Response struct {
	Publication                 Publication
	Session, Challenge, Started int
}

type Ticket struct {
	Session int
	Config  Config
}

type Subscriber struct {
	Session, Challenge int
	Started, Deadline  int
	Staged, Active     Config
	Applied            Publication
	Ticket             Ticket
	Connected          bool
}

type World struct {
	Now         int
	Publisher   Publication
	Subscribers [2]Subscriber
	// Transport buffers survive subscriber restarts so old-session replay is
	// actually explored. Only each recipient's authorized record is delivered.
	Pending [2]Response
}

type Action struct {
	Kind       string
	Subscriber int
}

func Initial() World {
	w := World{Publisher: Publication{Epoch: 1, Revision: 1}}
	for i := range w.Subscribers {
		w.Subscribers[i] = Subscriber{Session: 1, Connected: true}
		w.Publisher.Records[i] = Credential{Tenant: i + 1, Key: 1, Version: 1, Expires: 6}
	}
	return w
}

func Configuration(i, version int) Config {
	return Config{Revision: version, Binding: Binding{Tenant: i + 1, Backend: i + 1, Destination: i + 1, Key: 1, Version: version}}
}

func (w World) usable(i int, c Config) bool {
	s := w.Subscribers[i]
	r := s.Applied.Records[i]
	return c.Revision > 0 && c.Binding == Configuration(i, c.Binding.Version).Binding &&
		s.Applied.Epoch == 1 && w.Now < s.Deadline && r.Tenant == c.Binding.Tenant &&
		r.Key == c.Binding.Key && r.Version == c.Binding.Version && !r.Revoked && w.Now < r.Expires
}

// Activate requires exact credential-version compatibility. A staged incompatible
// config does not replace the active config; an unusable active config cannot serve.
func (w *World) Activate(i int) {
	s := &w.Subscribers[i]
	if w.usable(i, s.Staged) && s.Staged.Revision >= s.Active.Revision {
		s.Active = s.Staged
	}
}

func (w *World) ReceiveConfig(i int, c Config) bool {
	s := &w.Subscribers[i]
	if c.Revision < 1 || c.Binding != Configuration(i, c.Binding.Version).Binding ||
		c.Binding.Version < 1 || c.Revision < s.Staged.Revision ||
		(c.Revision == s.Staged.Revision && c != s.Staged) {
		return false
	}
	s.Staged = c
	w.Activate(i)
	return true
}

func (w *World) Receive(i int, response Response) bool {
	s := &w.Subscribers[i]
	p := response.Publication
	if s.Challenge == 0 || response.Session != s.Session || response.Challenge != s.Challenge ||
		response.Started != s.Started || p.Epoch != 1 || p.Revision < 1 ||
		w.Now >= response.Started+Lease || p.Revision < s.Applied.Revision ||
		(p.Revision == s.Applied.Revision && p != s.Applied) {
		return false
	}
	for index, c := range p.Records {
		if index != i && c != (Credential{}) {
			return false
		}
	}
	c := p.Records[i]
	if c != (Credential{}) && (c.Tenant != i+1 || c.Key != 1 || c.Version < 1) {
		return false
	}
	s.Applied = p
	s.Deadline = response.Started + Lease
	w.Activate(i)
	return true
}

// Resolve models an atomic local release decision, with no acquisition call.
func (w World) Resolve(i int, request Binding) (Ticket, bool) {
	s := w.Subscribers[i]
	if request != s.Active.Binding || !w.usable(i, s.Active) {
		return Ticket{}, false
	}
	return Ticket{Session: s.Session, Config: s.Active}, true
}

// Dispatch is a proposed stronger contract than the existing ext_proc fixture:
// recheck locally at dispatch; tickets cannot migrate across tenants, destinations,
// active configs or subscriber lifetimes. Actual Envoy dispatch is NOT wired here.
func (w World) Dispatch(i int, ticket Ticket, request Binding) bool {
	s := w.Subscribers[i]
	return ticket.Session == s.Session && ticket.Config == s.Active &&
		request == ticket.Config.Binding && w.usable(i, ticket.Config)
}

// Step returns an independent state; this keeps schedule exploration deterministic.
func (w World) Step(a Action) World {
	i := a.Subscriber
	s := &w.Subscribers[i]
	switch a.Kind {
	case "config1":
		w.ReceiveConfig(i, Configuration(i, 1))
	case "config2":
		w.ReceiveConfig(i, Configuration(i, 2))
	case "fetch":
		if s.Connected {
			s.Challenge++
			s.Started = w.Now
			p := w.Publisher
			p.Records[1-i] = Credential{}
			w.Pending[i] = Response{p, s.Session, s.Challenge, s.Started}
		}
	case "deliver":
		w.Receive(i, w.Pending[i])
	case "resolve":
		s.Ticket, _ = w.Resolve(i, s.Active.Binding)
	case "restart":
		// Session is a ghost representation of fresh unpredictable challenges,
		// not a proposed durable counter in an otherwise memory-only subscriber.
		*s = Subscriber{Session: s.Session + 1, Connected: s.Connected}
	case "disconnect":
		s.Connected = false
	case "reconnect":
		s.Connected = true
	case "tick":
		w.Now++
	case "rotate":
		if w.Publisher.Revision == 1 {
			w.Publisher.Revision = 2
			for j := range w.Publisher.Records {
				w.Publisher.Records[j].Version = 2
			}
		}
	case "revoke":
		w.Publisher.Revision = 3
		for j := range w.Publisher.Records {
			w.Publisher.Records[j].Revoked = true
		}
	default:
		panic("unknown action")
	}
	return w
}
