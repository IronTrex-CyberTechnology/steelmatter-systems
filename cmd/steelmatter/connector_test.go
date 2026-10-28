package main

import (
	"strings"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector/odoo"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestConnectorFromEnv(t *testing.T) {
	ok := map[string]string{"STEELMATTER_CONNECTOR": "odoo", "STEELMATTER_ODOO_URL": "http://127.0.0.1:18069", "STEELMATTER_ODOO_DB": "smr1",
		"STEELMATTER_ODOO_EXECUTOR_PASSWORD": "exec-secret-7f3a", "STEELMATTER_ODOO_OBSERVER_PASSWORD": "obs-secret-9c1d"}
	if c, kind, err := connectorFromEnv(env(map[string]string{})); c != nil || kind != "none" || err != nil {
		t.Fatalf("default: %v %s %v", c, kind, err)
	}
	c, kind, err := connectorFromEnv(env(ok))
	if err != nil || kind != "odoo" {
		t.Fatalf("odoo: %s %v", kind, err)
	}
	if _, isOdoo := c.(*odoo.Connector); !isOdoo {
		t.Fatalf("connector %T", c)
	}
	for name, change := range map[string]func(map[string]string){
		"missing password":  func(m map[string]string) { delete(m, "STEELMATTER_ODOO_OBSERVER_PASSWORD") },
		"same logins":       func(m map[string]string) { m["STEELMATTER_ODOO_OBSERVER_LOGIN"] = "sm_executor" },
		"remote plain http": func(m map[string]string) { m["STEELMATTER_ODOO_URL"] = "http://odoo.example.com" },
		"bad timeout":       func(m map[string]string) { m["STEELMATTER_ODOO_TIMEOUT_SECONDS"] = "-1" },
		"unknown kind":      func(m map[string]string) { m["STEELMATTER_CONNECTOR"] = "stub" },
	} {
		m := map[string]string{}
		for k, v := range ok {
			m[k] = v
		}
		change(m)
		if _, _, err := connectorFromEnv(env(m)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "secret-") {
			t.Errorf("%s: error leaks a password: %v", name, err)
		}
	}
}
