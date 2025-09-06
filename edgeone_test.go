package caddy_edgeone_ip

import (
    "context"
    "testing"
    "time"

    "github.com/caddyserver/caddy/v2"
    "github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func TestDefault(t *testing.T) {
    testDefault(t, `edgeone`)
    testDefault(t, `edgeone { }`)
}

func testDefault(t *testing.T, input string) {
    d := caddyfile.NewTestDispenser(input)

    r := EdgeOneIPRange{}
    err := r.UnmarshalCaddyfile(d)
    if err != nil {
        t.Errorf("unmarshal error for %q: %v", input, err)
    }

    ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
    defer cancel()

    err = r.Provision(ctx)
    if err != nil {
        t.Errorf("error provisioning %q: %v", input, err)
    }
}

func TestUnmarshal(t *testing.T) {
    input := `
    edgeone {
        urls https://example.com/ipv4.txt https://example.com/ipv6.txt
        interval 1.5h
        timeout 30s
    }`

    d := caddyfile.NewTestDispenser(input)

    r := EdgeOneIPRange{}
    err := r.UnmarshalCaddyfile(d)
    if err != nil {
        t.Errorf("unmarshal error: %v", err)
    }

    expectedInterval := caddy.Duration(90 * time.Minute)
    if expectedInterval != r.Interval {
        t.Errorf("incorrect interval: expected %v, got %v", expectedInterval, r.Interval)
    }

    expectedTimeout := caddy.Duration(30 * time.Second)
    if expectedTimeout != r.Timeout {
        t.Errorf("incorrect timeout: expected %v, got %v", expectedTimeout, r.Timeout)
    }

    if len(r.URLs) != 2 {
        t.Errorf("unexpected urls length: expected 2, got %d", len(r.URLs))
    }
}

// Simulates being nested in another block.
func TestUnmarshalNested(t *testing.T) {
    input := `{
                    edgeone {
                        urls https://example.com/ipv4.txt https://example.com/ipv6.txt
                        interval 1.5h
                        timeout 30s
                    }
                    other_module 10h
                }`

    d := caddyfile.NewTestDispenser(input)

    // Enter the outer block.
    d.Next()
    d.NextBlock(d.Nesting())

    r := EdgeOneIPRange{}
    err := r.UnmarshalCaddyfile(d)
    if err != nil {
        t.Errorf("unmarshal error: %v", err)
    }

    expectedInterval := caddy.Duration(90 * time.Minute)
    if expectedInterval != r.Interval {
        t.Errorf("incorrect interval: expected %v, got %v", expectedInterval, r.Interval)
    }

    expectedTimeout := caddy.Duration(30 * time.Second)
    if expectedTimeout != r.Timeout {
        t.Errorf("incorrect timeout: expected %v, got %v", expectedTimeout, r.Timeout)
    }

    if len(r.URLs) != 2 {
        t.Errorf("unexpected urls length: expected 2, got %d", len(r.URLs))
    }

    d.Next()
    if d.Val() != "other_module" {
        t.Errorf("cursor at unexpected position, expected 'other_module', got %v", d.Val())
    }
}

