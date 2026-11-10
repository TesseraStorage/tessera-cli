package main

import (
	"fmt"
	"os"
)

// dashboardAPI is the moneyd surface for browser dashboard links and
// signing out every session. Optional so existing billing fakes keep compiling.
type dashboardAPI interface {
	DashboardLink() (url string, err error)
	LogoutAll() error
}

func cmdDashboard(args []string) {
	if hasFlag(args, "--help") || hasFlag(args, "-h") {
		printDashboardHelp()
		return
	}
	pos := positional(args)
	logoutAll := hasFlag(args, "--logout-all") || (len(pos) > 0 && pos[0] == "logout-all")
	if logoutAll {
		cmdDashboardLogoutAll()
		return
	}
	cfg, err := loadConfig()
	if err != nil {
		if os.IsNotExist(err) {
			fatal("%s", errNoBillingCredentials)
		}
		fatal("read config: %v", err)
	}
	if credentialLabel(cfg) == "" {
		fatal("%s", errNoBillingCredentials)
	}
	api, ok := newBillingClient(cfg).(dashboardAPI)
	if !ok {
		fatal("this build cannot request a dashboard link")
	}
	url, err := api.DashboardLink()
	if err != nil {
		fatal("%s", describeBillingError(err))
	}
	fmt.Println(url)
	fmt.Println()
	fmt.Println("This link expires in 10 minutes.")
	fmt.Println("Open it in a browser and click Continue to dashboard.")
}

func cmdDashboardLogoutAll() {
	cfg, err := loadConfig()
	if err != nil {
		if os.IsNotExist(err) {
			fatal("%s", errNoBillingCredentials)
		}
		fatal("read config: %v", err)
	}
	if credentialLabel(cfg) == "" {
		fatal("%s", errNoBillingCredentials)
	}
	api, ok := newBillingClient(cfg).(dashboardAPI)
	if !ok {
		fatal("this build cannot sign out browser sessions")
	}
	if err := api.LogoutAll(); err != nil {
		fatal("%s", describeBillingError(err))
	}
	fmt.Println("Signed out every browser session for this account.")
	fmt.Println("This machine's CLI credentials are unchanged.")
}

func printDashboardHelp() {
	fmt.Println("Usage: tessera dashboard")
	fmt.Println("       tessera dashboard --logout-all")
	fmt.Println("       tessera dashboard logout-all")
	fmt.Println()
	fmt.Println("Print a one-time browser login link (valid 10 minutes).")
	fmt.Println("The link opens a confirmation page; click Continue to dashboard")
	fmt.Println("to sign in.")
	fmt.Println()
	fmt.Println("  --logout-all   end every browser session for this account")
	fmt.Println("                 (same as: tessera dashboard logout-all)")
}
