package wait

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/docker/go-connections/nat"
)

// Implement interface
var _ Strategy = (*HostPortStrategy)(nil)
var _ StrategyTimeout = (*HostPortStrategy)(nil)

type HostPortStrategy struct {
	// Port is a string containing port number and protocol in the format "80/tcp"
	// which
	Port nat.Port
	// all WaitStrategies should have a startupTimeout to avoid waiting infinitely
	timeout      *time.Duration
	PollInterval time.Duration
}

// NewHostPortStrategy constructs a default host port strategy
func NewHostPortStrategy(port nat.Port) *HostPortStrategy {
	return &HostPortStrategy{
		Port:         port,
		PollInterval: defaultPollInterval(),
	}
}

// fluent builders for each property
// since go has neither covariance nor generics, the return type must be the type of the concrete implementation
// this is true for all properties, even the "shared" ones like startupTimeout

// ForListeningPort is a helper similar to those in Wait.java
// https://github.com/testcontainers/testcontainers-java/blob/1d85a3834bd937f80aad3a4cec249c027f31aeb4/core/src/main/java/org/testcontainers/containers/wait/strategy/Wait.java
func ForListeningPort(port nat.Port) *HostPortStrategy {
	return NewHostPortStrategy(port)
}

// ForExposedPort constructs an exposed port strategy. Alias for `NewHostPortStrategy("")`.
// This strategy waits for the first port exposed in the Docker container.
func ForExposedPort() *HostPortStrategy {
	return NewHostPortStrategy("")
}

// WithStartupTimeout can be used to change the default startup timeout
func (hp *HostPortStrategy) WithStartupTimeout(startupTimeout time.Duration) *HostPortStrategy {
	hp.timeout = &startupTimeout
	return hp
}

// WithPollInterval can be used to override the default polling interval of 100 milliseconds
func (hp *HostPortStrategy) WithPollInterval(pollInterval time.Duration) *HostPortStrategy {
	hp.PollInterval = pollInterval
	return hp
}

func (hp *HostPortStrategy) Timeout() *time.Duration {
	return hp.timeout
}

// WaitUntilReady implements Strategy.WaitUntilReady
func (hp *HostPortStrategy) WaitUntilReady(ctx context.Context, target StrategyTarget) (err error) {
	timeout := defaultStartupTimeout()
	if hp.timeout != nil {
		timeout = *hp.timeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ipAddress, err := target.Host(ctx)
	if err != nil {
		return
	}

	var waitInterval = hp.PollInterval

	internalPort := hp.Port
	if internalPort == "" {
		var ports nat.PortMap
		ports, err = target.Ports(ctx)
		if err != nil {
			return
		}
		if len(ports) > 0 {
			for p := range ports {
				internalPort = p
				break
			}
		}
	}

	if internalPort == "" {
		err = fmt.Errorf("no port to wait for")
		return
	}

	var port nat.Port
	port, err = target.MappedPort(ctx, internalPort)
	var i = 0

	for port == "" {
		i++

		select {
		case <-ctx.Done():
			return fmt.Errorf("%s:%w", ctx.Err(), err)
		case <-time.After(waitInterval):
			if err := checkTarget(ctx, target); err != nil {
				return err
			}
			port, err = target.MappedPort(ctx, internalPort)
			if err != nil {
				fmt.Printf("(%d) [%s] %s\n", i, port, err)
			}
		}
	}

	proto := port.Proto()
	portNumber := port.Int()
	portString := strconv.Itoa(portNumber)

	//external check
	dialer := net.Dialer{}
	address := net.JoinHostPort(ipAddress, portString)
	for {
		if err := checkTarget(ctx, target); err != nil {
			return err
		}
		conn, err := dialer.DialContext(ctx, proto, address)
		if err != nil {
			if v, ok := err.(*net.OpError); ok {
				if v2, ok := (v.Err).(*os.SyscallError); ok {
					if isConnRefusedErr(v2.Err) {
						time.Sleep(waitInterval)
						continue
					}
				}
			}
			return err
		} else {
			_ = conn.Close()
			break
		}
	}

	//internal check
	command := buildInternalCheckCommand(internalPort.Int())
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := checkTarget(ctx, target); err != nil {
			return err
		}
		exitCode, _, err := target.Exec(ctx, []string{"/bin/sh", "-c", command})
		if err != nil {
			return fmt.Errorf("%w, host port waiting failed", err)
		}

		if exitCode == 0 {
			break
		} else if exitCode == 126 {
			return errors.New("/bin/sh command not executable")
		}
	}

	return nil
}

func buildInternalCheckCommand(internalPort int) string {
	command := `(
					cat /proc/net/tcp* | awk '{print $2}' | grep -i :%04x ||
					nc -vz -w 1 localhost %d ||
					/bin/sh -c '</dev/tcp/localhost/%d'
				)
				`
	return "true && " + fmt.Sprintf(command, internalPort, internalPort, internalPort)
}
