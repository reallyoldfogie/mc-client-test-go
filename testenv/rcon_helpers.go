package testenv

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type RCONHelper interface {
	RCON
	SetTime(ctx context.Context, spec string) *RCONResult
	SetWeather(ctx context.Context, weather string) *RCONResult
	SetGamerule(ctx context.Context, rule, value string) *RCONResult
	Teleport(ctx context.Context, target string, x, y, z float64) *RCONResult
	TeleportTo(ctx context.Context, target, destination string) *RCONResult
	Say(ctx context.Context, msg string) *RCONResult
	SetBlock(ctx context.Context, x, y, z int64, blockType, blockAction string) *RCONResult
	SummonEntity(ctx context.Context, x, y, z float64, entityType, nbtData string) *RCONResult

	GetEntityData(ctx context.Context, target, dataPath string) (parsedUser, data string, err error)
	GetEntityDimension(ctx context.Context, cmd string) string
	GetEntityPos(ctx context.Context, target string) (X, Y, Z float64, err error)

	ExecuteMany(ctx context.Context, cmds ...*RCONResult) (responses map[string]string, err error)
	ExecuteWithRetry(ctx context.Context, cmd string, maxRetries int) (string, error)
}

type RCONResult struct {
	helper RCONHelper
	Cmd    string
}

func (r *RCONResult) Exec(ctx context.Context) (string, error) {
	return r.helper.ExecuteWithRetry(ctx, r.Cmd, 3)
}

// rconHelper provides convenience methods on top of a raw RCON interface.
type rconHelper struct {
	RCON
	version string // Minecraft server version for syntax compatibility
}

// NewRCONHelper wraps a raw RCON client and auto-detects the server version.
func NewRCONHelper(r RCON) RCONHelper {
	h := &rconHelper{RCON: r, version: ""}
	// Try to auto-detect version from server
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if version := h.detectVersion(ctx); version != "" {
		h.version = version
		log.Printf("Auto-detected Minecraft version: %s", version)
	}
	return h
}

// detectVersion queries the server for its version string.
func (h *rconHelper) detectVersion(ctx context.Context) string {
	// Query server version using "list" command which returns version info
	result, err := h.RCON.Exec(ctx, "version")
	if err != nil {
		log.Printf("[rconHelper] Failed to detect version: %v", err)
		return ""
	}

	// The list command in newer versions includes version info
	// For reliability, we'll try to parse common version patterns
	// Extract version from output like "... running Minecraft version 1.21.11"
	re := regexp.MustCompile(`(?i)(?:version|minecraft\s+)([0-9]+\.[0-9]+\.[0-9]+)`)
	matches := re.FindStringSubmatch(result)
	if len(matches) > 1 {
		return matches[1]
	}

	// Fallback: try simpler pattern for older versions
	re2 := regexp.MustCompile(`([0-9]+\.[0-9]+(?:\.[0-9]+)?)`)
	matches = re2.FindStringSubmatch(result)
	if len(matches) > 1 {
		return matches[1]
	}

	return ""
}

// isVersionGreaterOrEqual checks if the helper's version is >= target version.
// Handles both legacy (1.21.x) and new (YY.x.x) versioning schemes.
func (h *rconHelper) isVersionGreaterOrEqual(targetVersion string) bool {
	if h.version == "" {
		return false // Unknown version, assume old format
	}

	parseVersion := func(v string) [3]int {
		parts := strings.Split(v, ".")
		var nums [3]int
		for i := 0; i < len(parts) && i < 3; i++ {
			fmt.Sscanf(parts[i], "%d", &nums[i])
		}
		return nums
	}

	current := parseVersion(h.version)
	target := parseVersion(targetVersion)

	if current[0] != target[0] {
		return current[0] > target[0]
	}
	if current[1] != target[1] {
		return current[1] > target[1]
	}
	return current[2] >= target[2]
}

func (r *rconHelper) ExecuteWithRetry(ctx context.Context, cmd string, maxRetries int) (string, error) {
	results := []string{}
	for i := range maxRetries {
		result, err := r.RCON.Exec(ctx, cmd)
		if err == nil {
			return result, nil
		}
		if strings.Contains(err.Error(), "EOF") || strings.Contains(result, "EOF") {
			results = append(results, result)
			log.Printf("RCON connection lost, reconnecting (attempt %d/%d)", i+1, maxRetries)
			err = r.Reconnect(ctx)
			if err != nil {
				log.Printf("RCON failed to reconnect (attempt %d/%d): %v", i+1, maxRetries, err)
			}
			continue
		}
		return result, err
	}
	return fmt.Sprintf("Exec failed after %d retries(%#v)", maxRetries, results), fmt.Errorf("failed after %d retries", maxRetries)
}

// SetTime sets the in-game time, e.g. "day", "noon", "night", or a tick value like "1000".
func (h *rconHelper) SetTime(ctx context.Context, spec string) *RCONResult {
	cmd := fmt.Sprintf("time set %s", spec)

	// _, err := h.Exec(ctx, cmd)
	return &RCONResult{Cmd: cmd, helper: h}
}

// SetWeather sets the weather, e.g. "clear", "rain", "thunder".
func (h *rconHelper) SetWeather(ctx context.Context, weather string) *RCONResult {
	cmd := fmt.Sprintf("weather %s", weather)
	// _, err := h.Exec(ctx, cmd)
	return &RCONResult{Cmd: cmd, helper: h}
}

// SetGamerule sets a gamerule to a given value.
// Automatically handles version-specific syntax:
// - 1.21.11+: Converts camelCase rule names to minecraft:snake_case
// - Pre-1.21.11: Uses original camelCase names
func (h *rconHelper) SetGamerule(ctx context.Context, rule, value string) *RCONResult {
	// Map of old rule names to new names for 1.21.11+
	newNameMap := map[string]string{
		"doDaylightCycle":   "minecraft:do_daylight_cycle",
		"doWeatherCycle":    "minecraft:do_weather_cycle",
		"doMobSpawning":     "minecraft:spawn_mobs",
		"doMobLoot":         "minecraft:mob_drops",
		"mobGriefing":       "minecraft:mob_griefing",
		"keepInventory":     "minecraft:keep_inventory",
	}

	var ruleNameToUse string
	if h.isVersionGreaterOrEqual("1.21.11") {
		// Use new syntax for 1.21.11+
		if newName, ok := newNameMap[rule]; ok {
			ruleNameToUse = newName
		} else {
			// For unmapped rules, convert using snake_case
			ruleNameToUse = fmt.Sprintf("minecraft:%s", ToMinecraftSnakeCase(rule))
		}
	} else {
		// Use old syntax for pre-1.21.11 versions
		ruleNameToUse = rule
	}

	cmd := fmt.Sprintf("gamerule %s %s", ruleNameToUse, value)
	return &RCONResult{Cmd: cmd, helper: h}
}

// Teleport teleports a target (player/entity selector) to coordinates.
func (h *rconHelper) Teleport(ctx context.Context, target string, x, y, z float64) *RCONResult {
	cmd := fmt.Sprintf("tp %s %.2f %.2f %.2f", target, x, y, z)
	// _, err := h.Exec(ctx, cmd)
	return &RCONResult{Cmd: cmd, helper: h}
}

// TeleportTo teleports a target to another player or selector.
func (h *rconHelper) TeleportTo(ctx context.Context, target, destination string) *RCONResult {
	cmd := fmt.Sprintf("tp %s %s", target, destination)
	// _, err := h.Exec(ctx, cmd)
	return &RCONResult{Cmd: cmd, helper: h}
}

// Say broadcasts a chat message from the server.
func (h *rconHelper) Say(ctx context.Context, msg string) *RCONResult {
	// Basic sanitization of newlines.
	cleaned := strings.TrimSpace(msg)
	cmd := fmt.Sprintf("say %s", cleaned)
	// _, err := h.Exec(ctx, cmd)
	return &RCONResult{Cmd: cmd, helper: h}
}

// SetBlock - sets the block at x, y, z to blockType
//
//	 -blockType should be a namespace qualified block id (i.e. minecraft:stone)
//	 -blockAction is one of the following:
//		-replace: The old block drops neither itself nor any contents. Plays no sound.
//		-destroy: The old block drops both itself and its contents (as if destroyed by a player). Plays the appropriate block breaking noise
//		-keep: Only air blocks are changed (non-air blocks are unchanged).
//		-strict: Place blocks as-is without triggering block updates and shape updates (1.21.5+)
//		-If not specified, defaults to replace.
func (h *rconHelper) SetBlock(ctx context.Context, x, y, z int64, blockType, blockAction string) *RCONResult {
	cmd := fmt.Sprintf("setblock %d %d %d %s %s", x, y, z, blockType, blockAction)
	// _, err := h.Exec(ctx, cmd)
	// if err != nil {
	// 	// return fmt.Errorf("[SetBlock][ERROR] %w", err)
	// 	return RCONResult{Err: err}

	// }

	return &RCONResult{Cmd: cmd, helper: h}
}

func (h *rconHelper) SummonEntity(ctx context.Context, x, y, z float64, entityType, nbtData string) *RCONResult {
	// summon block_display %f %f %f {block_state:{Name:"minecraft:diamond_block"}}
	cmd := fmt.Sprintf("summon %s %f %f %f %s", entityType, x, y, z, nbtData)
	return &RCONResult{Cmd: cmd, helper: h}
}

//	issues a data get entity command and parses the response:
//
// ReallyOldFogie has the following entity data: ['{"text":"User: ReallyOldFogie\\nPos: 770 62 330\\n "}']
// if dataPath is empty, Minecraft will return the full data for the target. Otherwise it will just return the data at that location in the entity data
func (h *rconHelper) GetEntityData(ctx context.Context, target, dataPath string) (parsedUser, data string, err error) {
	var itemStr string
	var user string
	var resp string

	cmd := fmt.Sprintf("data get entity %s %s", target, dataPath)
	resp, err = h.Exec(ctx, cmd)
	if err != nil {
		log.Printf("[ERROR] Failed to send command to minecraft: %s", err.Error())
		return "", "", err
	}

	re := regexp.MustCompile(`(?sm)(.*) has the following entity data: (.*)`)

	matches := re.FindAllStringSubmatch(resp, -1)
	if len(matches) == 1 {
		if len(matches[0]) == 3 {
			user = matches[0][1]
			itemStr = matches[0][2]
		} else {
			return "", "", fmt.Errorf("invalid response - parsed %d values, expected 3 [%v]", len(matches[0]), matches[0])
		}
	} else {
		return "", "", fmt.Errorf("invalid response - parsed %d values, expected 1 [%v]", len(matches), matches)
	}

	return user, itemStr, nil
}

func (h *rconHelper) GetEntityDimension(ctx context.Context, target string) string {
	dimension := "minecraft:overworld"
	_, dimension, _ = h.GetEntityData(ctx, target, "Dimension")
	dimension = strings.ReplaceAll(dimension, `"`, "")
	return dimension
}

func (h *rconHelper) GetEntityPos(ctx context.Context, target string) (X, Y, Z float64, err error) {
	parsedUser, data, err := h.GetEntityData(ctx, target, "Pos")
	if err != nil {
		return 0, 0, 0, err
	}

	data, _ = strings.CutPrefix(data, "[")
	data, _ = strings.CutSuffix(data, "]")
	data = strings.ReplaceAll(data, "d", "")
	data = strings.TrimSpace(data)

	log.Println("parsedUser", parsedUser, "data", data)

	posSlice := strings.Split(data, ",")

	for idx, val := range posSlice {
		posSlice[idx] = strings.TrimSpace(val)
	}

	X, err = strconv.ParseFloat(posSlice[0], 64)
	if err != nil {
		return
	}
	Y, err = strconv.ParseFloat(posSlice[1], 64)
	if err != nil {
		return
	}
	Z, err = strconv.ParseFloat(posSlice[2], 64)
	if err != nil {
		return
	}

	return X, Y, Z, nil
}

// ExecuteMany runs a series of commands, returning on the first error.
func (h *rconHelper) ExecuteMany(ctx context.Context, cmds ...*RCONResult) (map[string]string, error) {
	responses := map[string]string{}
	for _, c := range cmds {
		if strings.TrimSpace(c.Cmd) == "" {
			continue
		}
		if response, err := c.Exec(ctx); err != nil {
			return responses, fmt.Errorf("command %q failed: %w", c, err)
		} else {
			if existingResponse, ok := responses[c.Cmd]; ok {
				responses[c.Cmd] = existingResponse + "," + response
			} else {
				responses[c.Cmd] = response
			}
		}
	}
	return responses, nil
}

// ToMinecraftSnakeCase converts camelCase gamerules to snake_case.
// Example: "keepInventory" -> "keep_inventory"
func ToMinecraftSnakeCase(s string) string {
	// 1. Insert underscore before any uppercase letter that follows a lowercase letter or digit
	re := regexp.MustCompile("([a-z0-9])([A-Z])")
	snake := re.ReplaceAllString(s, "${1}_${2}")

	// 2. Convert the entire string to lowercase
	return strings.ToLower(snake)
}
