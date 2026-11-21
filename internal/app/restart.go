package app

import (
	"context"
	"fmt"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/runner"
	"github.com/aditya-shrivastavv/vbox-tui/internal/ssh"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// A graceful restart means the guest operating system reboots itself. The two
// ways to ask, and only these two:
//
//	ga   — VBoxManage controlvm reboot. VirtualBox asks the guest through the
//	       Guest Additions control service. Prerequisite: Additions installed
//	       and its service running in the guest; without it VBoxManage refuses,
//	       and the refusal is reported, never papered over. It is never a
//	       reset: controlvm reset exists precisely to fake a hardware power
//	       button and this app never calls it.
//	ssh  — one fixed reboot command from the table in internal/ssh, run
//	         through the VM's stored OpenSSH settings. Prerequisites: working
//		key-based SSH (OpenSSH owns authentication entirely; an interactive
//		password prompt does not exist for this call) and a known guest OS so
//		the command comes from the fixed table instead of a guess.
//
// A machine that can serve neither route is refused with its missing
// prerequisite named. Nothing here ever falls back to poweroff, power cycle,
// acpipowerbutton or reset.

// RestartMechanism is the chosen way to ask.
type RestartMechanism string

const (
	RestartViaGA  RestartMechanism = "ga"
	RestartViaSSH RestartMechanism = "ssh"
)

// RestartRoutes is everything the SSH route needs from the outside: stored
// settings plus the command runner. Both doors address VMs by UUID.
type RestartRoutes struct {
	SSH    func(uuid string) (config.SSHConfig, bool)
	Guest  func(uuid string) (config.GuestConfig, bool)
	Run    runner.Runner
	Client string // ssh program name, default ssh.ClientExe
}

// PlanRestart decides the mechanism for one VM and names the prerequisite
// that blocks it, if any. It is a pure decision: nothing is touched, and the
// batch preview and the actual run describe exactly the same rule.
//
// Rules, matching the stored modes: ga and ssh mean what they say and fail
// with their prerequisite when they cannot be honoured. The auto default
// prefers the SSH route when it is fully usable, because an SSH-capable guest
// without Additions is the common case; otherwise Guest Additions.
func PlanRestart(guest config.GuestConfig, sshCfg config.SSHConfig) (RestartMechanism, error) {
	switch guest.Restart {
	case config.GuestRestartOff:
		return "", fmt.Errorf("restart is switched off for this VM in its settings")
	case config.GuestRestartGA:
		return RestartViaGA, nil
	case config.GuestRestartSSH:
		if err := ssh.RestartPreflight(sshCfg, guest.OS); err != nil {
			return "", err
		}
		return RestartViaSSH, nil
	default: // auto
		if ssh.RestartPreflight(sshCfg, guest.OS) == nil {
			return RestartViaSSH, nil
		}
		return RestartViaGA, nil
	}
}

// RestartOutcome is how one restart request ended. Accepted and verified are
// different claims and the result line keeps them apart.
type RestartOutcome int

const (
	// RestartBlocked: the machine or its settings refused; nothing was sent.
	RestartBlocked RestartOutcome = iota
	// RestartRequestedGA: VirtualBox accepted the Additions reboot request.
	// Whether the guest completes it is the guest's decision from here.
	RestartRequestedGA
	// RestartVerifiedSSH: the guest ran the reboot command and confirmed it.
	RestartVerifiedSSH
	// RestartUncertainSSH: the connection dropped before confirmation, the
	// usual sound of a machine going down, but not proof.
	RestartUncertainSSH
	// RestartFailedSSH: the guest refused the command; nothing indicates a
	// reboot began.
	RestartFailedSSH
)

// RestartReport is the end of one restart request, honest about its class.
type RestartReport struct {
	Outcome   RestartOutcome
	Mechanism RestartMechanism
	// Why explains a block or failure (the missing prerequisite, the guest's
	// own refusal).
	Why string
}

// Summary is the result line for one machine.
func (r RestartReport) Summary(name string) string {
	switch r.Outcome {
	case RestartRequestedGA:
		return fmt.Sprintf("Restart requested for %s through the Guest Additions control service. The guest decides from there.", name)
	case RestartVerifiedSSH:
		return fmt.Sprintf("Restart requested for %s over SSH: the guest confirmed the reboot command.", name)
	case RestartUncertainSSH:
		return fmt.Sprintf("Restart requested for %s over SSH, but the connection dropped before the guest confirmed it. Check its console to be sure.", name)
	case RestartFailedSSH:
		return fmt.Sprintf("Restart of %s failed over SSH: %s", name, r.Why)
	default:
		return fmt.Sprintf("Cannot restart %s: %s. Nothing was changed.", name, r.Why)
	}
}

// OK reports whether the request actually reached a route that could work.
func (r RestartReport) OK() bool {
	switch r.Outcome {
	case RestartRequestedGA, RestartVerifiedSSH, RestartUncertainSSH:
		return true
	}
	return false
}

// RequestRestart asks one machine's guest OS, by UUID, to reboot itself.
// backend and routes supply the two mechanisms; the decision was already made
// by PlanRestart with the same inputs, so the plan shown to the user cannot
// disagree with what runs.
func RequestRestart(
	ctx context.Context,
	backend vbox.Manager,
	target vbox.Machine,
	routes RestartRoutes,
) RestartReport {
	mechanism, err := PlanRestart(guestFor(routes, target.UUID), sshFor(routes, target.UUID))
	if err != nil {
		return RestartReport{Outcome: RestartBlocked, Why: err.Error()}
	}
	switch mechanism {
	case RestartViaSSH:
		sshCfg, _ := routes.SSH(target.UUID)
		guest, _ := routes.Guest(target.UUID)
		res := ssh.RunRestart(ctx, routes.Run, routes.Client, sshCfg.Normalise(), guest.OS)
		switch res.Outcome {
		case ssh.RestartVerified:
			return RestartReport{Outcome: RestartVerifiedSSH, Mechanism: mechanism}
		case ssh.RestartUncertain:
			return RestartReport{Outcome: RestartUncertainSSH, Mechanism: mechanism}
		default:
			why := "the guest refused the reboot command"
			if res.Err != nil {
				why = res.Err.Error()
			}
			return RestartReport{Outcome: RestartFailedSSH, Mechanism: mechanism, Why: why}
		}
	default: // RestartViaGA — VirtualBox reports a missing Additions channel
		// as an error, which becomes the honest "why".
		if err := backend.GuestReboot(ctx, target.UUID); err != nil {
			return RestartReport{
				Outcome:   RestartBlocked,
				Mechanism: mechanism,
				Why: fmt.Sprintf("VirtualBox could not deliver the guest-reboot request (%s). This route needs Guest Additions with its control service running in the guest; a machine without them can only be restarted over SSH or by its own console. Nothing was reset or power-cycled.",
					oneError(err, 240)),
			}
		}
		return RestartReport{Outcome: RestartRequestedGA, Mechanism: mechanism}
	}
}

func guestFor(r RestartRoutes, uuid string) config.GuestConfig {
	if r.Guest == nil {
		return config.GuestConfig{}
	}
	g, _ := r.Guest(uuid)
	return g
}

func sshFor(r RestartRoutes, uuid string) config.SSHConfig {
	if r.SSH == nil {
		return config.SSHConfig{}
	}
	s, _ := r.SSH(uuid)
	return s
}
