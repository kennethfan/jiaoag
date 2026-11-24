package server

import (
	"testing"

	"github.com/kennethfan/jiaoag/internal/protocol"
)

func TestSessionCreateAcceptsReadOnlyRoots(t *testing.T) {
	ts := newModelsServer(t)
	c := dial(t, wsURL(ts), testToken)
	_ = readEnvelope(t, c)
	hello, err := protocol.New(protocol.TypeClientHello, protocol.ClientHelloPayload{
		ClientVersion:   "test-client",
		ProtocolVersion: protocol.Version,
	})
	if err != nil {
		t.Fatalf("构造 client.hello: %v", err)
	}
	write(t, c, hello)

	lib := t.TempDir()
	create, err := protocol.New(protocol.TypeSessionCreate, protocol.SessionCreatePayload{
		Workspace: t.TempDir(),
		Roots:     []protocol.SessionRootPayload{{Alias: "lib", Path: lib}},
	})
	if err != nil {
		t.Fatalf("构造 session.create: %v", err)
	}
	write(t, c, create)

	env := readEnvelope(t, c)
	if env.Type != protocol.TypeSessionCreated {
		t.Fatalf("期望 session.created，得到 %s：%s", env.Type, env.Payload)
	}
	var sess protocol.SessionPayload
	if err := env.Decode(&sess); err != nil {
		t.Fatalf("解析 session.created: %v", err)
	}
	if len(sess.Roots) != 1 || sess.Roots[0].Alias != "lib" {
		t.Fatalf("回包未带附加根：%+v", sess.Roots)
	}
}

func TestSessionCreateRejectsUnknownRootAlias(t *testing.T) {
	ts := newModelsServer(t)
	c := dial(t, wsURL(ts), testToken)
	_ = readEnvelope(t, c)
	hello, err := protocol.New(protocol.TypeClientHello, protocol.ClientHelloPayload{
		ClientVersion:   "test-client",
		ProtocolVersion: protocol.Version,
	})
	if err != nil {
		t.Fatalf("构造 client.hello: %v", err)
	}
	write(t, c, hello)

	create, err := protocol.New(protocol.TypeSessionCreate, protocol.SessionCreatePayload{
		Workspace: t.TempDir(),
		Roots:     []protocol.SessionRootPayload{{Alias: "Lib", Path: t.TempDir()}},
	})
	if err != nil {
		t.Fatalf("构造 session.create: %v", err)
	}
	write(t, c, create)

	env := readEnvelope(t, c)
	if env.Type != protocol.TypeError {
		t.Fatalf("期望 error，得到 %s", env.Type)
	}
}
