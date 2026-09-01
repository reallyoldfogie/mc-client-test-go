// Xgo:build integration

package mcclienttest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/reallyoldfogie/mc-client-test-go/testenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientCompatibility(t *testing.T) {
	versions := []string{
		// "1.20.4",
		"1.21.5",
	}

	mgr, err := testenv.NewManager()
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	for _, v := range versions {
		v := v
		t.Run("version_"+v, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()

			cfg := testenv.ServerConfig{
				Version:    v,
				PullImage:  true,       // pull image if not present
				OnlineMode: false,      // offline for tests
				NamePrefix: "mc-test-", // optional
				ExtraEnv: map[string]string{
					"DIFFICULTY": "peaceful",
					"MODE":       "creative",
				},
			}

			inst, err := mgr.Start(ctx, cfg)
			if err != nil {
				t.Fatalf("start server: %v", err)
			}

			// Always try to stop/remove, even on failure.
			// defer func() {
			// 	stopCtx, cancelStop := context.WithTimeout(context.Background(), 30*time.Second)
			// 	defer cancelStop()
			// 	t.Logf("Stopping instance %s", inst.Name)
			// 	if err := mgr.Stop(stopCtx, inst, true); err != nil {
			// 		t.Logf("warning: failed to stop/remove %s: %v", inst.Name, err)
			// 	}
			// }()

			t.Logf("Started server %s on %s:%d (RCON %d)",
				inst.Version, inst.Host, inst.HostServerPort, inst.HostRCONPort)

			if err := mgr.WaitReady(ctx, inst); err != nil {
				t.Fatalf("server not ready: %v", err)
			}
			t.Logf("Server %s is ready", inst.Version)

			// Use RCON to put the world in a known state.
			r, err := mgr.RCONClient(ctx, inst)
			if err != nil {
				t.Fatalf("rcon client: %v", err)
			}
			defer r.Close()

			helper := testenv.NewRCONHelper(r)

			resp, err := helper.Exec(ctx, `summon armor_stand 0 80 10 {CustomName:'{"text":"TestBot"}'}`)
			require.NoError(t, err)
			fmt.Println("resp", resp)

			setTimeCmd := helper.SetTime(ctx, "day")
			setWeatherCmd := helper.SetWeather(ctx, "clear")
			setGameruleCmd := helper.SetGamerule(ctx, "doDaylightCycle", "false")
			teleportCmd := helper.Teleport(ctx, `@e[type=minecraft:armor_stand,name="TestBot",limit=1]`, 0, 80, 0)

			if responses, err := helper.ExecuteMany(ctx, setTimeCmd, setWeatherCmd, setGameruleCmd, teleportCmd); err != nil {
				t.Fatal(err)
			} else {
				for cmd, response := range responses {
					fmt.Println(cmd, "=>", response)
				}
			}

			playerX, playerY, playerZ, err := helper.GetEntityPos(ctx, `@e[type=minecraft:armor_stand,name="TestBot",limit=1]`)
			if err != nil {
				t.Fatalf("failed to get player position: %v", err)
			}
			assert.Equal(t, float64(0), playerX, "X should match")
			assert.Equal(t, float64(80), playerY, "Y should match")
			assert.Equal(t, float64(0), playerZ, "Z should match")

			// TODO: Hook up your actual Minecraft client here.
			if err := runDummyClientCheck(inst.Host, inst.HostServerPort); err != nil {
				t.Fatalf("client check failed: %v", err)
			}
		})
	}
}

// runDummyClientCheck is a placeholder; replace with your real client.
func runDummyClientCheck(host string, port int) error {
	// Setup your client to connect to host:port and run a short scenario.
	// Return an error if anything fails.
	fmt.Printf("Pretend client connecting to %s:%d\n", host, port)
	return nil
}
