package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector/odoo"
)

// connectorFromEnv chooses the R1 destination connector for serve:
//
//	STEELMATTER_CONNECTOR=none (default)  no destination: R1 settlement and the restore check do not run
//	STEELMATTER_CONNECTOR=odoo            the Odoo 18 connector (core/connector/odoo), configured by
//	  STEELMATTER_ODOO_URL                 base URL (plain http only for a loopback host)
//	  STEELMATTER_ODOO_DB                  Odoo database name
//	  STEELMATTER_ODOO_EXECUTOR_LOGIN      default sm_executor
//	  STEELMATTER_ODOO_EXECUTOR_PASSWORD   required
//	  STEELMATTER_ODOO_OBSERVER_LOGIN      default sm_observer
//	  STEELMATTER_ODOO_OBSERVER_PASSWORD   required; a different login from the executor
//	  STEELMATTER_ODOO_TIMEOUT_SECONDS     per call, default 30
//
// The stub connector is for tests only and is never offered here.
func connectorFromEnv(getenv func(string) string) (connector.Connector, string, error) {
	switch kind := getenv("STEELMATTER_CONNECTOR"); kind {
	case "", "none":
		return nil, "none", nil
	case "odoo":
		cfg := odoo.Config{
			URL:      getenv("STEELMATTER_ODOO_URL"),
			Database: getenv("STEELMATTER_ODOO_DB"),
			Executor: odoo.Credentials{Login: orDefault(getenv("STEELMATTER_ODOO_EXECUTOR_LOGIN"), "sm_executor"),
				Password: getenv("STEELMATTER_ODOO_EXECUTOR_PASSWORD")},
			Observer: odoo.Credentials{Login: orDefault(getenv("STEELMATTER_ODOO_OBSERVER_LOGIN"), "sm_observer"),
				Password: getenv("STEELMATTER_ODOO_OBSERVER_PASSWORD")},
			Timeout:  30 * time.Second,
			Instance: "odoo",
		}
		for name, v := range map[string]string{"STEELMATTER_ODOO_URL": cfg.URL, "STEELMATTER_ODOO_DB": cfg.Database,
			"STEELMATTER_ODOO_EXECUTOR_PASSWORD": cfg.Executor.Password, "STEELMATTER_ODOO_OBSERVER_PASSWORD": cfg.Observer.Password} {
			if v == "" {
				return nil, "", fmt.Errorf("%s is required with STEELMATTER_CONNECTOR=odoo", name)
			}
		}
		if cfg.Executor.Login == cfg.Observer.Login {
			return nil, "", fmt.Errorf("the Odoo executor and observer must be different logins")
		}
		if v := getenv("STEELMATTER_ODOO_TIMEOUT_SECONDS"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return nil, "", fmt.Errorf("STEELMATTER_ODOO_TIMEOUT_SECONDS must be a positive integer")
			}
			cfg.Timeout = time.Duration(n) * time.Second
		}
		c, err := odoo.New(cfg)
		if err != nil {
			return nil, "", fmt.Errorf("odoo connector: %w", err)
		}
		return c, "odoo", nil
	default:
		return nil, "", fmt.Errorf("STEELMATTER_CONNECTOR must be none or odoo, not %q", kind)
	}
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
