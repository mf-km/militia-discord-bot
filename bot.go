package main

// =============================================================================
// IMPORTS
// =============================================================================
import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/robfig/cron/v3"
)

// =============================================================================
// CONFIGURATION
// =============================================================================
// Config holds all values loaded from config.json at startup.
// Adding a new configurable value: add the field here, add it to config.json
// and config.example.json, then reference it as cfg.FieldName below.
type Config struct {
	Token                string   `json:"token"`
	BotCommandsChannel   string   `json:"botCommandsChannel"`
	AlliancePingsChannel string   `json:"alliancePingsChannel"`
	IC24PingsChannels    []string `json:"ic24PingsChannels"`
	RelayWebhookURLs     []string `json:"relayWebhookURLs"`
}

// cfg is the package-level config instance, populated by loadConfig() in main()
var cfg Config

// loadConfig reads config.json from the same directory as the binary.
// Exits immediately if the file is missing or invalid — the bot cannot
// run without its configuration.
func loadConfig() {
	data, err := os.ReadFile("config.json")
	if err != nil {
		fmt.Println("Error reading config.json:", err)
		os.Exit(1)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		fmt.Println("Error parsing config.json:", err)
		os.Exit(1)
	}
	fmt.Println("Config loaded successfully.")
}

// =============================================================================
// PACKAGE-LEVEL GLOBALS
// =============================================================================
// scheduler:         the running cron instance, started in main()
// scheduledPings:    in-memory list of active scheduled pings, kept in sync with schedules.json
// nextPingID:        auto-incrementing ID counter for scheduled pings
// session:           the active Discord session, set in main() after login
// pendingPinglater:  holds unconfirmed .pinglater requests keyed by Discord user ID
var (
	scheduler        *cron.Cron
	scheduledPings   []ScheduledPing
	nextPingID       = 1
	session          *discordgo.Session
	pendingPinglater = map[string]PendingPing{}
)

// PendingPing holds a .pinglater request awaiting y/n confirmation.
// ExpiresAt is checked on every message — if now is past ExpiresAt the pending
// state is discarded and the user has to run .pinglater again.
type PendingPing struct {
	Target    string
	CronExpr  string
	Message   string
	UnixTime  int64
	ExpiresAt time.Time
}

// =============================================================================
// SEND HELPERS
// =============================================================================

// sendAlliance sends to the single alliance pings channel.
func sendAlliance(message string) {
	_, err := session.ChannelMessageSendComplex(cfg.AlliancePingsChannel, &discordgo.MessageSend{
		Content: message,
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{
				discordgo.AllowedMentionTypeEveryone,
				discordgo.AllowedMentionTypeRoles,
			},
		},
	})
	if err != nil {
		fmt.Println("Failed to send alliance message:", err)
	}
}

// sendIC24 sends to all channel IDs in ic24PingsChannels.
func sendIC24(message string) {
	for _, ch := range cfg.IC24PingsChannels {
		_, err := session.ChannelMessageSendComplex(ch, &discordgo.MessageSend{
			Content: message,
			AllowedMentions: &discordgo.MessageAllowedMentions{
				Parse: []discordgo.AllowedMentionType{
					discordgo.AllowedMentionTypeEveryone,
					discordgo.AllowedMentionTypeRoles,
				},
			},
		})
		if err != nil {
			fmt.Println("Failed to send IC24 message to", ch, ":", err)
		}
	}
}

// sendMilitia sends to all webhook URLs in relayWebhookURLs via HTTP POST.
// No bot token or server membership required — just the webhook URL.
func sendMilitia(message string) {
	payload, _ := json.Marshal(map[string]interface{}{
		"content": message,
		"allowed_mentions": map[string]interface{}{
			"parse": []string{"everyone", "roles"},
		},
	})
	for _, webhookURL := range cfg.RelayWebhookURLs {
		resp, err := http.Post(webhookURL, "application/json", bytes.NewBuffer(payload))
		if err != nil {
			fmt.Println("Failed to post to webhook:", err)
			continue
		}
		resp.Body.Close()
	}
}

// sendByTarget is a convenience dispatcher used by scheduled jobs and
// loadSchedules so they don't need a switch statement everywhere.
func sendByTarget(target, message string) {
	switch target {
	case "alliance":
		sendAlliance(message)
	case "militia":
		sendMilitia(message)
	case "ic24":
		sendIC24(message)
	}
}

// =============================================================================
// SCHEDULED PING MANAGEMENT
// =============================================================================

// registerCronJob hands a recurring job to the cron scheduler.
// Returns the EntryID needed to remove the job later.
func registerCronJob(target, cronExpr, message string) (cron.EntryID, error) {
	return scheduler.AddFunc(cronExpr, func() {
		sendByTarget(target, message)
	})
}

// addScheduledPing registers a new recurring cron job, builds the record,
// appends it to scheduledPings, and increments nextPingID.
func addScheduledPing(target, cronExpr, message string) (ScheduledPing, error) {
	cronID, err := registerCronJob(target, cronExpr, message)
	if err != nil {
		return ScheduledPing{}, err
	}
	p := ScheduledPing{
		ScheduledPingRecord: ScheduledPingRecord{
			ID:      nextPingID,
			Cron:    cronExpr,
			Target:  target,
			Message: message,
			Once:    false,
		},
		CronID: cronID,
	}
	scheduledPings = append(scheduledPings, p)
	nextPingID++
	return p, nil
}

// addOneTimePing registers a one-shot job. The closure sends the message then
// calls removeScheduledPing and saveSchedules to clean itself up.
func addOneTimePing(target, cronExpr, message string) (ScheduledPing, error) {
	p := ScheduledPing{
		ScheduledPingRecord: ScheduledPingRecord{
			ID:      nextPingID,
			Cron:    cronExpr,
			Target:  target,
			Message: message,
			Once:    true,
		},
	}
	id := p.ID
	cronID, err := scheduler.AddFunc(cronExpr, func() {
		sendByTarget(target, message)
		removeScheduledPing(id)
		if err := saveSchedules(); err != nil {
			fmt.Println("Warning: failed to save schedules after one-shot fired:", err)
		}
	})
	if err != nil {
		return ScheduledPing{}, err
	}
	p.CronID = cronID
	scheduledPings = append(scheduledPings, p)
	nextPingID++
	return p, nil
}

// removeScheduledPing finds a ping by ID, removes it from the scheduler,
// and removes it from the in-memory slice. Returns true if found.
func removeScheduledPing(id int) bool {
	for i, p := range scheduledPings {
		if p.ID == id {
			scheduler.Remove(p.CronID)
			scheduledPings = append(scheduledPings[:i], scheduledPings[i+1:]...)
			return true
		}
	}
	return false
}

// =============================================================================
// MAIN
// =============================================================================
func main() {
	// ---------------------------------------------------------
	// Load config.json before anything else
	// ---------------------------------------------------------
	loadConfig()

	// ---------------------------------------------------------
	// Start the cron scheduler before loading saved schedules
	// ---------------------------------------------------------
	scheduler = cron.New()
	scheduler.Start()
	loadSchedules()

	// ---------------------------------------------------------
	// Create the Discord session using token from config
	// ---------------------------------------------------------
	var err error
	session, err = discordgo.New("Bot " + cfg.Token)
	if err != nil {
		fmt.Println("Error creating session:", err)
		os.Exit(1)
	}

	// ---------------------------------------------------------
	// MESSAGE HANDLER
	// Gates on author == bot and wrong channel first, then
	// checks for pending confirmations before command parsing.
	// ---------------------------------------------------------
	session.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {

		// Ignore the bot's own messages
		if m.Author.ID == s.State.User.ID {
			return
		}

		// Only process messages in the bot commands channel
		if m.ChannelID != cfg.BotCommandsChannel {
			return
		}

		// =================================================
		// PENDING PINGLATER CONFIRMATION CHECK
		// Runs before command parsing so a bare "y" or "n"
		// from a user with a pending confirmation doesn't
		// fall through to the unrecognized command path.
		// =================================================
		if pending, ok := pendingPinglater[m.Author.ID]; ok {
			if time.Now().After(pending.ExpiresAt) {
				delete(pendingPinglater, m.Author.ID)
				s.ChannelMessageSend(m.ChannelID, "⏱️ Confirmation timed out. Run `.pinglater` again.")
				return
			}

			switch strings.TrimSpace(strings.ToLower(m.Content)) {
			case "y":
				delete(pendingPinglater, m.Author.ID)
				p, err := addOneTimePing(pending.Target, pending.CronExpr, pending.Message)
				if err != nil {
					s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("❌ Failed to schedule ping: %v", err))
					return
				}
				if err := saveSchedules(); err != nil {
					fmt.Println("Warning: failed to save schedules:", err)
				}
				s.ChannelMessageSend(m.ChannelID, fmt.Sprintf(
					"✅ One-time ping **#%d** scheduled for <t:%d:F> (<t:%d:R>).",
					p.ID, pending.UnixTime, pending.UnixTime,
				))
			case "n":
				delete(pendingPinglater, m.Author.ID)
				s.ChannelMessageSend(m.ChannelID, "❌ Cancelled. Run `.pinglater` again with a different time.")
			default:
				s.ChannelMessageSend(m.ChannelID, fmt.Sprintf(
					"⚠️ You have a pending ping scheduled for <t:%d:F>. Reply `y` to confirm or `n` to cancel (expires <t:%d:R>).",
					pending.UnixTime, pending.ExpiresAt.Unix(),
				))
			}
			return
		}

		// =================================================
		// COMMANDS
		// To add a new command:
		//   1. Add a new } else if block following the pattern below
		//   2. Use strings.HasPrefix for commands that take arguments
		//      or m.Content == for exact-match commands
		//   3. Add it to the .help response at the bottom
		// =================================================

		// -------------------------------------------------
		// .ping <message>
		// Sends to alliance channel. Deletes the command message.
		// -------------------------------------------------
		if strings.HasPrefix(m.Content, ".ping ") {
			message := strings.TrimPrefix(m.Content, ".ping ")
			s.ChannelMessageDelete(m.ChannelID, m.ID)
			sendAlliance(message)

		// -------------------------------------------------
		// .pingmil <message>
		// Sends to all webhook URLs in relayWebhookURLs.
		// Deletes the command message.
		// -------------------------------------------------
		} else if strings.HasPrefix(m.Content, ".pingmil ") {
			message := strings.TrimPrefix(m.Content, ".pingmil ")
			s.ChannelMessageDelete(m.ChannelID, m.ID)
			sendMilitia(message)

		// -------------------------------------------------
		// .pingic24 <message>
		// Sends to all channel IDs in ic24PingsChannels.
		// Deletes the command message.
		// -------------------------------------------------
		} else if strings.HasPrefix(m.Content, ".pingic24 ") {
			message := strings.TrimPrefix(m.Content, ".pingic24 ")
			s.ChannelMessageDelete(m.ChannelID, m.ID)
			sendIC24(message)

		// -------------------------------------------------
		// .pinglater alliance|militia|ic24 YYYYMMDD HH:MM <message>
		// Schedules a one-time future ping. Time is UTC.
		// Bot replies with a confirmation prompt showing the
		// time in the user's local timezone via Discord timestamp.
		// User has 5 minutes to reply y or n.
		// -------------------------------------------------
		} else if strings.HasPrefix(m.Content, ".pinglater ") {
			parts := strings.SplitN(strings.TrimPrefix(m.Content, ".pinglater "), " ", 4)
			if len(parts) < 4 {
				s.ChannelMessageSend(m.ChannelID, "Usage: `.pinglater alliance|militia|ic24 YYYYMMDD HH:MM <message>`\nExample: `.pinglater alliance 20260414 19:00 @everyone Fleet time!`\nAll times are UTC.")
				return
			}
			target := parts[0]
			if target != "alliance" && target != "militia" && target != "ic24" {
				s.ChannelMessageSend(m.ChannelID, "❌ Target must be `alliance`, `militia`, or `ic24`.")
				return
			}
			dateStr := parts[1]
			timeStr := parts[2]
			message := parts[3]

			t, err := time.ParseInLocation("20060102 15:04", dateStr+" "+timeStr, time.UTC)
			if err != nil {
				s.ChannelMessageSend(m.ChannelID, "❌ Invalid date/time. Use `YYYYMMDD HH:MM` — example: `20260414 19:00`")
				return
			}
			if t.Before(time.Now()) {
				s.ChannelMessageSend(m.ChannelID, "❌ That time is in the past.")
				return
			}

			cronExpr := fmt.Sprintf("%d %d %d %d *", t.Minute(), t.Hour(), t.Day(), int(t.Month()))

			if _, exists := pendingPinglater[m.Author.ID]; exists {
				s.ChannelMessageSend(m.ChannelID, "⚠️ Your previous unconfirmed `.pinglater` has been replaced.")
			}

			pendingPinglater[m.Author.ID] = PendingPing{
				Target:    target,
				CronExpr:  cronExpr,
				Message:   message,
				UnixTime:  t.Unix(),
				ExpiresAt: time.Now().Add(5 * time.Minute),
			}

			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf(
				"📅 Schedule this ping for <t:%d:F> (<t:%d:R>) to **%s**?\n> %s\nReply `y` to confirm or `n` to cancel. This prompt expires in 5 minutes.",
				t.Unix(), t.Unix(), target, message,
			))

		// -------------------------------------------------
		// .pingschedule add alliance|militia|ic24 <min> <hour> <dom> <month> <dow> <message>
		// Adds a recurring scheduled ping using a standard cron expression.
		// Cron fields: min hour day-of-month month day-of-week
		// Example: 0 19 * * 1 = every Monday at 19:00 UTC
		// -------------------------------------------------
		} else if strings.HasPrefix(m.Content, ".pingschedule add ") {
			parts := strings.SplitN(strings.TrimPrefix(m.Content, ".pingschedule add "), " ", 7)
			if len(parts) < 7 {
				s.ChannelMessageSend(m.ChannelID, "Usage: `.pingschedule add alliance|militia|ic24 <min> <hour> <dom> <month> <dow> <message>`\nExample: `.pingschedule add alliance 0 19 * * 1 @everyone Fleet in 1 hour!`")
				return
			}
			target := parts[0]
			if target != "alliance" && target != "militia" && target != "ic24" {
				s.ChannelMessageSend(m.ChannelID, "❌ Target must be `alliance`, `militia`, or `ic24`.")
				return
			}
			cronExpr := strings.Join(parts[1:6], " ")
			message := parts[6]

			p, err := addScheduledPing(target, cronExpr, message)
			if err != nil {
				s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("❌ Invalid cron expression: %v", err))
				return
			}
			if err := saveSchedules(); err != nil {
				fmt.Println("Warning: failed to save schedules:", err)
			}
			nextRun := scheduler.Entry(p.CronID).Next
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf(
				"✅ Scheduled ping **#%d** added.\n**Target:** %s\n**Cron:** `%s`\n**Message:** %s\n**Next run:** <t:%d:R>",
				p.ID, p.Target, p.Cron, p.Message, nextRun.Unix(),
			))

		// -------------------------------------------------
		// .pingschedule remove <id>
		// Removes a scheduled ping by ID. Get IDs from .pingschedule list.
		// -------------------------------------------------
		} else if strings.HasPrefix(m.Content, ".pingschedule remove ") {
			idStr := strings.TrimPrefix(m.Content, ".pingschedule remove ")
			id, err := strconv.Atoi(strings.TrimSpace(idStr))
			if err != nil {
				s.ChannelMessageSend(m.ChannelID, "Usage: `.pingschedule remove <id>`")
				return
			}
			if removeScheduledPing(id) {
				if err := saveSchedules(); err != nil {
					fmt.Println("Warning: failed to save schedules:", err)
				}
				s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("✅ Scheduled ping **#%d** removed.", id))
			} else {
				s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("❌ No scheduled ping with ID **%d** found.", id))
			}

		// -------------------------------------------------
		// .pingschedule list
		// Lists all scheduled pings — both recurring and one-time.
		// Next run time uses Discord's <t:unix:R> relative format.
		// -------------------------------------------------
		} else if m.Content == ".pingschedule list" {
			if len(scheduledPings) == 0 {
				s.ChannelMessageSend(m.ChannelID, "No scheduled pings configured.")
				return
			}
			var sb strings.Builder
			sb.WriteString("**Scheduled Pings:**\n")
			for _, p := range scheduledPings {
				nextRun := scheduler.Entry(p.CronID).Next
				oneShot := ""
				if p.Once {
					oneShot = " *(one-time)*"
				}
				sb.WriteString(fmt.Sprintf(
					"**#%d**%s | `%s` | %s | next: <t:%d:R>\n> %s\n\n",
					p.ID, oneShot, p.Cron, p.Target, nextRun.Unix(), p.Message,
				))
			}
			s.ChannelMessageSend(m.ChannelID, sb.String())

		// -------------------------------------------------
		// .help
		// Update this any time you add a new command above.
		// -------------------------------------------------
		} else if m.Content == ".help" {
			s.ChannelMessageSend(m.ChannelID, "**Available commands:**\n"+
				"`.ping <message>` — posts to #alliance-pings\n"+
				"`.pingmil <message>` — posts to all militia relay webhooks\n"+
				"`.pingic24 <message>` — posts to IC24 channel list\n"+
				"`.pinglater alliance|militia|ic24 YYYYMMDD HH:MM <message>` — schedule a one-time ping (UTC, confirms before scheduling)\n"+
				"`.pingschedule add alliance|militia|ic24 <min> <hour> <dom> <month> <dow> <message>` — add a recurring scheduled ping\n"+
				"`.pingschedule remove <id>` — remove a scheduled ping by ID\n"+
				"`.pingschedule list` — show all scheduled pings and next run times\n"+
				"`.help` — this message")
		}

		// -------------------------------------------------
		// ADD NEW COMMANDS ABOVE THIS LINE
		// Pattern for a command with arguments:
		//   } else if strings.HasPrefix(m.Content, ".mycommand ") {
		//       args := strings.TrimPrefix(m.Content, ".mycommand ")
		//       s.ChannelMessageSend(m.ChannelID, "response")
		//
		// Pattern for an exact-match command (no arguments):
		//   } else if m.Content == ".mycommand" {
		//       s.ChannelMessageSend(m.ChannelID, "response")
		// -------------------------------------------------

	}) // end AddHandler

	// Tell Discord we only want guild message events
	session.Identify.Intents = discordgo.IntentsGuildMessages

	// Open the WebSocket connection
	err = session.Open()
	if err != nil {
		fmt.Println("Error opening connection:", err)
		os.Exit(1)
	}

	fmt.Println("Bot is running. Press CTRL+C to stop.")

	// Block until SIGINT or SIGTERM then shut down cleanly
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM)
	<-sc

	scheduler.Stop()
	session.Close()
}
