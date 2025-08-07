package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	"github.com/matthew-balzan/dca"
)

const (
	pageSize = 25 // Discord select menus support max 25 options
)

var (
	allowedExts = map[string]struct{}{
		".mp3":  {},
		".wav":  {},
		".flac": {},
		".ogg":  {},
		".m4b":  {},
	}

	soundsDir = getenv("SOUNDS_DIR", "./sounds")

	// Per user+guild ephemeral browser state
	browserStates = struct {
		sync.Mutex
		data map[string]*browserState
	}{data: make(map[string]*browserState)}

	// Playback sessions per guild
	playSessions sync.Map // map[guildID]*guildPlayback
)

type browserState struct {
	Files        []string // sorted, relative to soundsDir
	Page         int
	SelectedFile string
}

type guildPlayback struct {
	mu       sync.Mutex
	guildID  string
	vc       *discordgo.VoiceConnection
	enc      *dca.EncodeSession
	doneChan chan error
	playing  string
}

func (gp *guildPlayback) stop() {
	gp.mu.Lock()
	defer gp.mu.Unlock()

	// Best-effort stop: kill ffmpeg and disconnect VC.
	if gp.enc != nil {
		gp.enc.Cleanup()
		gp.enc = nil
	}
	if gp.vc != nil {
		_ = gp.vc.Speaking(false)
		_ = gp.vc.Disconnect()
		gp.vc = nil
	}
}

func main() {
	// Load .env (if present). Ignore error so missing .env is non-fatal.
	_ = godotenv.Load() // looks for ".env" in the current working directory

	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_TOKEN is not set. Put it in your environment or create a .env file with DISCORD_TOKEN=yourtoken")
	}

	if _, err := os.Stat(soundsDir); os.IsNotExist(err) {
		log.Printf("Warning: sounds directory %q does not exist (create it and add audio files)", soundsDir)
	}

	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatalf("failed to create discord session: %v", err)
	}

	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildVoiceStates

	dg.AddHandler(onInteractionCreate)

	if err := dg.Open(); err != nil {
		log.Fatalf("failed to open session: %v", err)
	}
	defer dg.Close()

	// Register slash commands
	appID := dg.State.User.ID
	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "sounds",
			Description: "Browse and play a local sound file",
		},
		{
			Name:        "stop",
			Description: "Stop playback and leave the voice channel",
		},
	}

	for _, cmd := range commands {
		if _, err := dg.ApplicationCommandCreate(appID, "", cmd); err != nil {
			log.Printf("Failed to register command /%s: %v", cmd.Name, err)
		}
	}

	log.Printf("Bot is running. Commands: /sounds, /stop")
	waitForSignal()

	// Cleanup on shutdown
	log.Println("Shutting down: stopping active playbacks")
	playSessions.Range(func(key, value any) bool {
		if gp, ok := value.(*guildPlayback); ok {
			gp.stop()
		}
		return true
	})
}

func onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		data := i.ApplicationCommandData()
		switch data.Name {
		case "sounds":
			handleSoundsCommand(s, i)
		case "stop":
			handleStopCommand(s, i)
		}
	case discordgo.InteractionMessageComponent:
		handleComponent(s, i)
	}
}

func intPtr(i int) *int { return &i }

// /sounds -> ephemeral paginated file picker
func handleSoundsCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	files, err := listAudioFiles(soundsDir)
	if err != nil {
		respondEphemeral(s, i, fmt.Sprintf("Error scanning sounds: %v", err), nil)
		return
	}
	if len(files) == 0 {
		respondEphemeral(s, i, "No audio files found in "+soundsDir, nil)
		return
	}

	key := browserKey(i)
	browserStates.Lock()
	browserStates.data[key] = &browserState{
		Files: files,
		Page:  0,
	}
	state := browserStates.data[key]
	browserStates.Unlock()

	content := "Select a sound to play"
	components := buildSoundPickerComponents(state)
	respondEphemeral(s, i, content, components)
}

func handleStopCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	gid := i.GuildID
	val, ok := playSessions.Load(gid)
	if !ok {
		respondEphemeral(s, i, "Nothing is playing.", nil)
		return
	}
	gp := val.(*guildPlayback)
	gp.stop()
	playSessions.Delete(gid)
	respondEphemeral(s, i, "Stopped playback and left the voice channel.", nil)
}

func handleComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.MessageComponentData()
	key := browserKey(i)

	switch data.CustomID {
	case "sounds_prev", "sounds_next", "sounds_cancel":
		browserStates.Lock()
		state, ok := browserStates.data[key]
		browserStates.Unlock()
		if !ok {
			respondUpdate(s, i, "Session expired. Run /sounds again.", nil)
			return
		}
		switch data.CustomID {
		case "sounds_prev":
			if state.Page > 0 {
				state.Page--
			}
			respondUpdate(s, i, "Select a sound to play", buildSoundPickerComponents(state))
		case "sounds_next":
			maxPage := (len(state.Files) - 1) / pageSize
			if state.Page < maxPage {
				state.Page++
			}
			respondUpdate(s, i, "Select a sound to play", buildSoundPickerComponents(state))
		case "sounds_cancel":
			// End the ephemeral browser
			browserStates.Lock()
			delete(browserStates.data, key)
			browserStates.Unlock()
			respondUpdate(s, i, "Cancelled.", []discordgo.MessageComponent{})
		}
	case "sound_select":
		// selection value = index into state.Files
		browserStates.Lock()
		state, ok := browserStates.data[key]
		browserStates.Unlock()
		if !ok {
			respondUpdate(s, i, "Session expired. Run /sounds again.", nil)
			return
		}
		vals := data.Values
		if len(vals) == 0 {
			respondUpdate(s, i, "No selection received. Try again.", buildSoundPickerComponents(state))
			return
		}
		idx, err := strconv.Atoi(vals[0])
		if err != nil || idx < 0 || idx >= len(state.Files) {
			respondUpdate(s, i, "Invalid selection. Try again.", buildSoundPickerComponents(state))
			return
		}
		state.SelectedFile = state.Files[idx]
		// Move to voice channel selection view
		components := buildVoiceChannelPickerComponents(s, i.GuildID)
		content := fmt.Sprintf("Selected: %s\nSelect a voice channel to join and play.", state.SelectedFile)
		respondUpdate(s, i, content, components)
	case "back_to_sounds":
		browserStates.Lock()
		state, ok := browserStates.data[key]
		browserStates.Unlock()
		if !ok {
			respondUpdate(s, i, "Session expired. Run /sounds again.", nil)
			return
		}
		state.SelectedFile = ""
		respondUpdate(s, i, "Select a sound to play", buildSoundPickerComponents(state))
	case "voice_select":
		// Start playback
		browserStates.Lock()
		state, ok := browserStates.data[key]
		browserStates.Unlock()
		if !ok || state.SelectedFile == "" {
			respondUpdate(s, i, "Session expired or no sound selected. Run /sounds again.", nil)
			return
		}
		vals := data.Values
		if len(vals) == 0 {
			respondUpdate(s, i, "No channel selected.", buildVoiceChannelPickerComponents(s, i.GuildID))
			return
		}
		channelID := vals[0]
		relPath := state.SelectedFile
		fullPath := filepath.Join(soundsDir, relPath)

		go func() {
			if err := startPlayback(s, i.GuildID, channelID, fullPath); err != nil {
				log.Printf("playback error: %v", err)
			}
		}()
		msg := fmt.Sprintf("Joining <#%s> and playing: %s\nUse /stop to stop and disconnect.", channelID, relPath)
		respondUpdate(s, i, msg, []discordgo.MessageComponent{})
	default:
		// Unknown component
		respondUpdate(s, i, "Unsupported interaction.", nil)
	}
}

// Quick decode probe (verifies the file can be read/decoded)
func probeDecode(file string) error {
	var stderr bytes.Buffer
	cmd := exec.Command(
		"ffmpeg",
		"-v", "error",
		"-nostdin",
		"-hide_banner",
		"-ss", "0",
		"-t", "3",
		"-i", file,
		"-f", "null", "-",
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg decode probe failed: %v; stderr:\n%s", err, stderr.String())
	}
	if s := strings.TrimSpace(stderr.String()); s != "" {
		log.Printf("[probeDecode] ffmpeg stderr (warnings):\n%s", s)
	}
	return nil
}

// Opus encode probe (verifies ffmpeg has an opus encoder like libopus)
// dca typically relies on ffmpeg producing opus frames when RawOutput=true.
func probeOpusEncode(file string) error {
	var stderr bytes.Buffer
	cmd := exec.Command(
		"ffmpeg",
		"-v", "error",
		"-nostdin",
		"-hide_banner",
		"-i", file,
		"-t", "1",
		"-c:a", "libopus", // try libopus explicitly
		"-f", "ogg", "NUL", // Windows null sink; on Linux use /dev/null
	)
	// If you are on Linux, replace "NUL" with "/dev/null"
	if runtime.GOOS != "windows" {
		cmd = exec.Command(
			"ffmpeg", "-v", "error", "-nostdin", "-hide_banner",
			"-i", file, "-t", "1", "-c:a", "libopus", "-f", "ogg", "/dev/null",
		)
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg opus-encode probe failed (libopus likely missing): %v; stderr:\n%s", err, stderr.String())
	}
	return nil
}

func startPlayback(s *discordgo.Session, guildID, channelID, filePath string) error {
	log.Printf("[startPlayback] requested: guild=%s channel=%s file=%s", guildID, channelID, filePath)

	// Try to log channel info (type/name)
	if ch, err := s.State.Channel(channelID); err == nil && ch != nil {
		log.Printf("[startPlayback] channel info: name=%q type=%v", ch.Name, ch.Type)
	}

	// File check
	info, err := os.Stat(filePath)
	if err != nil {
		log.Printf("[startPlayback] file stat error: %v", err)
		return fmt.Errorf("file not accessible: %w", err)
	}
	log.Printf("[startPlayback] file exists: %s (size=%d bytes)", filePath, info.Size())

	// ffmpeg presence
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Printf("[startPlayback] ffmpeg not found in PATH: %v", err)
		return fmt.Errorf("ffmpeg not found in PATH: %w", err)
	}
	log.Printf("[startPlayback] ffmpeg found on PATH")

	// Probes
	if err := probeDecode(filePath); err != nil {
		log.Printf("[startPlayback] decode probe error: %v", err)
		return err
	}
	if err := probeOpusEncode(filePath); err != nil {
		log.Printf("[startPlayback] opus encode probe error: %v", err)
		log.Printf("[startPlayback] Tip: your ffmpeg likely lacks libopus. Install a full build (e.g., winget install Gyan.FFmpeg or choco install ffmpeg).")
		return err
	}

	// Stop existing session in this guild if any
	if val, ok := playSessions.Load(guildID); ok {
		old := val.(*guildPlayback)
		log.Printf("[startPlayback] stopping existing playback for guild=%s", guildID)
		old.stop()
		playSessions.Delete(guildID)
	}

	// Join voice: mute=false, deaf=false
	log.Printf("[startPlayback] joining voice channel %s in guild %s", channelID, guildID)
	vc, err := s.ChannelVoiceJoin(guildID, channelID, false, false)
	if err != nil {
		log.Printf("[startPlayback] ChannelVoiceJoin error: %v", err)
		return fmt.Errorf("failed to join voice channel: %w", err)
	}
	log.Printf("[startPlayback] joined voice; waiting for readiness")

	// Wait for the voice connection to be ready
	for i := 0; i < 50; i++ {
		if vc.Ready && vc.OpusSend != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !vc.Ready || vc.OpusSend == nil {
		log.Printf("[startPlayback] voice connection not ready after wait: Ready=%v OpusSendNil=%v", vc.Ready, vc.OpusSend == nil)
		_ = vc.Disconnect()
		return fmt.Errorf("voice connection not ready (Ready=%v, OpusSend nil=%v)", vc.Ready, vc.OpusSend == nil)
	}
	log.Printf("[startPlayback] voice connection ready")

	// Encoder options
	opts := dca.StdEncodeOptions
	opts.RawOutput = false // <-- THE FIX: Let dca handle Opus encoding.
	opts.Bitrate = 320     // kbps
	//opts.Volume = 256      // This is the default volume, good to have explicitly.

	log.Printf("[startPlayback] starting encoder for file %s", filePath)
	enc, err := dca.EncodeFile(filePath, opts)
	if err != nil {
		log.Printf("[startPlayback] EncodeFile error: %v", err)
		_ = vc.Disconnect()
		return fmt.Errorf("failed to start ffmpeg/dca encode for %q: %w", filePath, err)
	}
	log.Printf("[startPlayback] encoder started successfully")

	done := make(chan error, 1)

	// Save playback session
	gp := &guildPlayback{
		guildID:  guildID,
		vc:       vc,
		enc:      enc,
		doneChan: done,
		playing:  filePath,
	}
	playSessions.Store(guildID, gp)

	log.Printf("[startPlayback] launching playback lifecycle goroutine")

	// Use a single goroutine for the entire playback lifecycle.
	go func() {
		// Defer cleanup tasks to run when this goroutine finishes.
		defer func() {
			log.Printf("[startPlayback] stream lifecycle finished, cleaning up...")
			_ = vc.Speaking(false)
			enc.Cleanup()
			_ = vc.Disconnect()
			playSessions.Delete(guildID)
			log.Printf("[startPlayback] playback session cleaned up for guild=%s", guildID)
		}()

		// Set speaking status
		if err := vc.Speaking(true); err != nil {
			log.Printf("[startPlayback] vc.Speaking(true) error: %v", err)
		}

		// The dca.NewStream function is a blocking call that streams audio.
		// It will send an error to the 'done' channel when it's finished.
		dca.NewStream(enc, vc, done)

		// Wait for the 'done' channel to receive the result from NewStream.
		err = <-done
		if err != nil && err != io.EOF {
			log.Printf("[startPlayback] stream finished with an unexpected error: %v", err)
		} else {
			log.Printf("[startPlayback] stream finished successfully (EOF)")
		}
	}()

	log.Printf("[startPlayback] started playback for guild=%s channel=%s file=%s", guildID, channelID, filePath)
	return nil
}

func buildSoundPickerComponents(state *browserState) []discordgo.MessageComponent {
	start := state.Page * pageSize
	if start > len(state.Files) {
		start = len(state.Files)
	}
	end := start + pageSize
	if end > len(state.Files) {
		end = len(state.Files)
	}
	options := make([]discordgo.SelectMenuOption, 0, end-start)
	for idx := start; idx < end; idx++ {
		label := displayName(state.Files[idx])
		// Ensure label under 100 chars
		if len(label) > 100 {
			label = label[:100]
		}
		options = append(options, discordgo.SelectMenuOption{
			Label: label,
			Value: strconv.Itoa(idx),
		})
	}

	maxPage := (len(state.Files) - 1) / pageSize
	prevDisabled := state.Page <= 0
	nextDisabled := state.Page >= maxPage

	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.SelectMenu{
					CustomID:    "sound_select",
					Placeholder: "Pick a sound",
					MinValues:   intPtr(1),
					MaxValues:   1,
					Options:     options,
				},
			},
		},
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					CustomID: "sounds_prev",
					Label:    "Prev",
					Style:    discordgo.SecondaryButton,
					Disabled: prevDisabled,
				},
				discordgo.Button{
					CustomID: "sounds_next",
					Label:    "Next",
					Style:    discordgo.SecondaryButton,
					Disabled: nextDisabled,
				},
				discordgo.Button{
					CustomID: "sounds_cancel",
					Label:    "Cancel",
					Style:    discordgo.DangerButton,
				},
			},
		},
	}
}

func buildVoiceChannelPickerComponents(s *discordgo.Session, guildID string) []discordgo.MessageComponent {
	chans, err := s.GuildChannels(guildID)
	if err != nil {
		// In case of error, return only a back button
		return []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{
						CustomID: "back_to_sounds",
						Label:    "Back",
						Style:    discordgo.SecondaryButton,
					},
				},
			},
		}
	}

	var voiceChans []*discordgo.Channel
	for _, c := range chans {
		if c.Type == discordgo.ChannelTypeGuildVoice || c.Type == discordgo.ChannelTypeGuildStageVoice {
			voiceChans = append(voiceChans, c)
		}
	}

	// Sort by position/name
	sort.SliceStable(voiceChans, func(i, j int) bool {
		if voiceChans[i].Position == voiceChans[j].Position {
			return strings.ToLower(voiceChans[i].Name) < strings.ToLower(voiceChans[j].Name)
		}
		return voiceChans[i].Position < voiceChans[j].Position
	})

	// Build up to 25 options
	max := len(voiceChans)
	if max > pageSize {
		max = pageSize
	}
	options := make([]discordgo.SelectMenuOption, 0, max)
	for idx := 0; idx < max; idx++ {
		c := voiceChans[idx]
		label := c.Name
		if len(label) > 100 {
			label = label[:100]
		}
		options = append(options, discordgo.SelectMenuOption{
			Label: label,
			Value: c.ID,
		})
	}

	rows := []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.SelectMenu{
					CustomID:    "voice_select",
					Placeholder: "Pick a voice channel",
					MinValues:   intPtr(1),
					MaxValues:   1,
					Options:     options,
				},
			},
		},
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					CustomID: "back_to_sounds",
					Label:    "Back",
					Style:    discordgo.SecondaryButton,
				},
			},
		},
	}

	return rows
}

func respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string, components []discordgo.MessageComponent) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content:    content,
			Flags:      discordgo.MessageFlagsEphemeral,
			Components: components,
		},
	})
}

func respondUpdate(s *discordgo.Session, i *discordgo.InteractionCreate, content string, components []discordgo.MessageComponent) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:    content,
			Components: components,
		},
	})
}

func browserKey(i *discordgo.InteractionCreate) string {
	uid := ""
	if i.Member != nil && i.Member.User != nil {
		uid = i.Member.User.ID
	} else if i.User != nil {
		uid = i.User.ID
	}
	return uid + ":" + i.GuildID
}

func listAudioFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Skip unreadable subtrees but continue scanning others
			return nil
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if _, ok := allowedExts[ext]; ok {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				rel = d.Name()
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func displayName(rel string) string {
	// Show relative path without extension
	base := rel
	if idx := strings.LastIndex(rel, "."); idx > 0 {
		base = rel[:idx]
	}
	return base
}

func waitForSignal() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
