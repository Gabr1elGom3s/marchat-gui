package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/Cod-e-Codes/marchat/client/config"
	"github.com/Cod-e-Codes/marchat/client/crypto"
	"github.com/Cod-e-Codes/marchat/shared"
	"github.com/gorilla/websocket"
)

const (
	maxMessages         = 100
	maxUsersDisplay     = 20
	pingPeriod          = 50 * time.Second
	reconnectMaxDelay   = 30 * time.Second
	userListMinWidth    = 180
	inputMinHeight      = 80
	defaultWindowWidth  = 1000
	defaultWindowHeight = 700
)

var (
	mentionRegex *regexp.Regexp
	urlRegex     *regexp.Regexp
)

func init() {
	mentionRegex = regexp.MustCompile(`\B@([a-zA-Z0-9_]+)\b`)
	urlRegex = regexp.MustCompile(`(https?://[^\s<>"{}|\\^` + "`" + `\[\]]+|www\.[^\s<>"{}|\\^` + "`" + `\[\]]+\.[a-zA-Z]{2,})`)

	// Set up debug logger
	f, err := os.OpenFile("marchat-gui-debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		log.SetOutput(f)
	} else {
		log.SetOutput(os.Stderr)
	}
}

// getKeystorePath returns the standard keystore path following OS conventions
func getKeystorePath() string {
	// Get the user's config directory following OS conventions
	configDir, err := os.UserConfigDir()
	if err != nil {
		// Fallback to a reasonable default
		configDir = filepath.Join(os.Getenv("HOME"), ".config")
		if runtime.GOOS == "windows" {
			configDir = filepath.Join(os.Getenv("APPDATA"))
		}
	}

	// Create marchat subdirectory
	marchatDir := filepath.Join(configDir, "marchat")

	// Ensure the directory exists
	os.MkdirAll(marchatDir, 0700)

	// Return the keystore file path
	return filepath.Join(marchatDir, "keystore.dat")
}

// MarchatGUI represents the main GUI application
type MarchatGUI struct {
	app      fyne.App
	window   fyne.Window
	cfg      *config.Config
	keystore *crypto.KeyStore

	// WebSocket connection
	conn           *websocket.Conn
	connected      bool
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	reconnectDelay time.Duration

	// GUI Components
	chatList         *widget.List
	messageContainer *fyne.Container   // For scroll approach
	chatScroll       *container.Scroll // Reference to scroll container
	userList         *widget.List
	messageEntry     *widget.Entry
	statusLabel      *widget.Label
	sendButton       *widget.Button

	// Data
	messages      []shared.Message
	users         []string
	receivedFiles map[string]*shared.FileMeta

	// State
	sending           bool
	twentyFourHour    bool
	selectedUserIndex int
	selectedUser      string

	// Admin features
	isAdmin  bool
	adminKey string

	// Dialogs and windows (using interface{} since dialog types vary by version)
	helpDialog        interface{}
	adminDialog       interface{}
	codeSnippetDialog interface{}
	filePickerDialog  interface{}

	// Bell notifications
	bellEnabled     bool
	bellOnMention   bool
	lastBellTime    time.Time
	minBellInterval time.Duration

	// Synchronization
	mu sync.RWMutex
}

// NewMarchatGUI creates a new GUI instance
func NewMarchatGUI(cfg *config.Config, keystore *crypto.KeyStore, isAdmin bool, adminKey string) *MarchatGUI {
	log.Printf("Creating GUI instance...")

	fyneApp := app.NewWithID("com.marchat.client")
	if fyneApp == nil {
		log.Fatal("Failed to create Fyne app")
	}
	log.Printf("Created Fyne app")

	// Set theme safely
	themeName := "system" // default
	if cfg != nil && cfg.Theme != "" {
		themeName = cfg.Theme
	}
	log.Printf("Setting theme: %s", themeName)
	fyneApp.Settings().SetTheme(getThemeFromConfig(themeName))
	log.Printf("Theme set successfully")

	// Create window safely
	username := "User"
	if cfg != nil && cfg.Username != "" {
		username = cfg.Username
	}
	log.Printf("Creating window for user: %s", username)
	window := fyneApp.NewWindow("marchat - " + username)
	if window == nil {
		log.Fatal("Failed to create window")
	}
	log.Printf("Window created successfully")

	log.Printf("Resizing window...")
	window.Resize(fyne.NewSize(defaultWindowWidth, defaultWindowHeight))
	log.Printf("Setting window icon...")
	window.SetIcon(theme.ComputerIcon()) // Use built-in icon
	log.Printf("Window setup complete")

	gui := &MarchatGUI{
		app:               fyneApp,
		window:            window,
		cfg:               cfg,
		keystore:          keystore,
		isAdmin:           isAdmin,
		adminKey:          adminKey,
		receivedFiles:     make(map[string]*shared.FileMeta),
		reconnectDelay:    time.Second,
		selectedUserIndex: -1,
		twentyFourHour:    cfg != nil && cfg.TwentyFourHour,
		bellEnabled:       true,  // Default to true
		bellOnMention:     false, // Default to false
		minBellInterval:   500 * time.Millisecond,
	}

	log.Printf("Setting up UI...")
	gui.setupUI()
	log.Printf("UI setup complete")

	log.Printf("Setting up keyboard shortcuts...")
	gui.setupKeyboardShortcuts()
	log.Printf("Keyboard shortcuts setup complete")

	log.Printf("GUI instance created successfully")
	return gui
}

// NewMarchatGUIWithApp creates a new GUI instance using an existing Fyne app
func NewMarchatGUIWithApp(fyneApp fyne.App, cfg *config.Config, keystore *crypto.KeyStore, isAdmin bool, adminKey string) *MarchatGUI {
	log.Printf("Creating GUI instance with existing app...")

	// Set theme safely
	themeName := "system" // default
	if cfg != nil && cfg.Theme != "" {
		themeName = cfg.Theme
	}
	log.Printf("Setting theme: %s", themeName)
	fyneApp.Settings().SetTheme(getThemeFromConfig(themeName))
	log.Printf("Theme set successfully")

	// Create window safely
	username := "User"
	if cfg != nil && cfg.Username != "" {
		username = cfg.Username
	}
	log.Printf("Creating window for user: %s", username)
	window := fyneApp.NewWindow("marchat - " + username)
	if window == nil {
		log.Fatal("Failed to create window")
	}
	log.Printf("Window created successfully")

	log.Printf("Resizing window...")
	window.Resize(fyne.NewSize(defaultWindowWidth, defaultWindowHeight))
	log.Printf("Setting window icon...")
	window.SetIcon(theme.ComputerIcon()) // Use built-in icon
	log.Printf("Window setup complete")

	gui := &MarchatGUI{
		app:               fyneApp,
		window:            window,
		cfg:               cfg,
		keystore:          keystore,
		isAdmin:           isAdmin,
		adminKey:          adminKey,
		receivedFiles:     make(map[string]*shared.FileMeta),
		reconnectDelay:    time.Second,
		selectedUserIndex: -1,
		twentyFourHour:    cfg != nil && cfg.TwentyFourHour,
		bellEnabled:       true,  // Default to true
		bellOnMention:     false, // Default to false
		minBellInterval:   500 * time.Millisecond,
	}

	log.Printf("Setting up UI...")
	gui.setupUI()
	log.Printf("UI setup complete")

	log.Printf("Setting up keyboard shortcuts...")
	gui.setupKeyboardShortcuts()
	log.Printf("Keyboard shortcuts setup complete")

	log.Printf("GUI instance created successfully with existing app")
	return gui
}

// NewMarchatGUIWithWindow creates a new GUI instance using an existing window
func NewMarchatGUIWithWindow(fyneApp fyne.App, window fyne.Window, cfg *config.Config, keystore *crypto.KeyStore, isAdmin bool, adminKey string) *MarchatGUI {
	log.Printf("Creating GUI instance with existing window...")

	// Set theme safely
	themeName := "system" // default
	if cfg != nil && cfg.Theme != "" {
		themeName = cfg.Theme
	}
	log.Printf("Setting theme: %s", themeName)
	fyneApp.Settings().SetTheme(getThemeFromConfig(themeName))
	log.Printf("Theme set successfully")

	// Update window title
	username := "User"
	if cfg != nil && cfg.Username != "" {
		username = cfg.Username
	}
	log.Printf("Updating window title for user: %s", username)
	window.SetTitle("marchat - " + username)

	log.Printf("Resizing window...")
	window.Resize(fyne.NewSize(defaultWindowWidth, defaultWindowHeight))
	log.Printf("Setting window icon...")
	window.SetIcon(theme.ComputerIcon()) // Use built-in icon
	log.Printf("Window setup complete")

	gui := &MarchatGUI{
		app:               fyneApp,
		window:            window,
		cfg:               cfg,
		keystore:          keystore,
		isAdmin:           isAdmin,
		adminKey:          adminKey,
		receivedFiles:     make(map[string]*shared.FileMeta),
		reconnectDelay:    time.Second,
		selectedUserIndex: -1,
		twentyFourHour:    cfg != nil && cfg.TwentyFourHour,
		bellEnabled:       true,  // Default to true
		bellOnMention:     false, // Default to false
		minBellInterval:   500 * time.Millisecond,
	}

	log.Printf("Setting up UI...")
	gui.setupUI()
	log.Printf("UI setup complete")

	log.Printf("Setting up keyboard shortcuts...")
	gui.setupKeyboardShortcuts()
	log.Printf("Keyboard shortcuts setup complete")

	log.Printf("GUI instance created successfully with existing window")
	log.Printf("GUI keystore instance: %p", gui.keystore)
	return gui
}

// getThemeFromConfig converts config theme to Fyne theme
func getThemeFromConfig(themeName string) fyne.Theme {
	switch strings.ToLower(themeName) {
	case "dark", "modern", "retro":
		return theme.DarkTheme()
	case "light", "system":
		return theme.LightTheme()
	default:
		return theme.DefaultTheme()
	}
}

// setupUI initializes the user interface components
func (gui *MarchatGUI) setupUI() {
	log.Printf("Starting UI setup...")

	// Status bar
	log.Printf("Creating status label...")
	gui.statusLabel = widget.NewLabel("Connecting...")
	if gui.statusLabel == nil {
		log.Fatal("Failed to create status label")
	}
	gui.statusLabel.Wrapping = fyne.TextWrapWord
	log.Printf("Status label created successfully")

	// Chat container using scroll approach (better for variable-height messages)
	log.Printf("Creating chat scroll container...")
	gui.messageContainer = container.NewVBox()
	gui.chatScroll = container.NewScroll(gui.messageContainer)
	gui.chatScroll.SetMinSize(fyne.NewSize(400, 300))

	// Initialize with existing messages
	gui.mu.RLock()
	for _, msg := range gui.messages {
		messageWidget := gui.createScrollMessageWidget(msg.Sender, msg.Content, msg.CreatedAt, msg.Type, msg.File)
		gui.messageContainer.Add(messageWidget)
	}
	gui.mu.RUnlock()

	// Scroll to bottom initially to show newest messages
	gui.chatScroll.ScrollToBottom()

	log.Printf("Chat scroll container created successfully")

	// User list
	log.Printf("Creating user list...")
	gui.userList = widget.NewList(
		func() int {
			gui.mu.RLock()
			defer gui.mu.RUnlock()
			return len(gui.users)
		},
		func() fyne.CanvasObject {
			label := widget.NewLabel("")
			label.Wrapping = fyne.TextWrapWord
			return label
		},
		func(i widget.ListItemID, obj fyne.CanvasObject) {
			gui.mu.RLock()
			if i >= len(gui.users) {
				gui.mu.RUnlock()
				return
			}
			user := gui.users[i]
			gui.mu.RUnlock()

			label := obj.(*widget.Label)
			if gui.cfg != nil && user == gui.cfg.Username {
				label.SetText("• " + user + " (me)")
				label.Importance = widget.MediumImportance
			} else {
				prefix := "• "
				if gui.isAdmin && gui.selectedUserIndex == i {
					prefix = "► "
					label.Importance = widget.HighImportance
				} else {
					label.Importance = widget.LowImportance
				}
				label.SetText(prefix + user)
			}
		},
	)
	if gui.userList == nil {
		log.Fatal("Failed to create user list")
	}
	log.Printf("User list created successfully")

	// User list selection for admin
	if gui.isAdmin {
		gui.userList.OnSelected = func(id widget.ListItemID) {
			gui.mu.Lock()
			if id < len(gui.users) && (gui.cfg == nil || gui.users[id] != gui.cfg.Username) {
				gui.selectedUserIndex = id
				gui.selectedUser = gui.users[id]
				gui.updateStatus(fmt.Sprintf("Selected user: %s", gui.selectedUser))
			} else {
				gui.selectedUserIndex = -1
				gui.selectedUser = ""
			}
			gui.mu.Unlock()
			gui.userList.Refresh()
		}
	}

	// Message entry
	log.Printf("Creating message entry...")
	gui.messageEntry = widget.NewMultiLineEntry()
	if gui.messageEntry == nil {
		log.Fatal("Failed to create message entry")
	}
	gui.messageEntry.SetPlaceHolder("Type your message...")
	gui.messageEntry.OnSubmitted = gui.sendMessage
	gui.messageEntry.Resize(fyne.NewSize(0, inputMinHeight))
	log.Printf("Message entry created successfully")

	// Send button
	log.Printf("Creating send button...")
	gui.sendButton = widget.NewButton("Send", func() { gui.sendMessage("") })
	if gui.sendButton == nil {
		log.Fatal("Failed to create send button")
	}
	gui.sendButton.Importance = widget.HighImportance
	log.Printf("Send button created successfully")

	// Input container
	log.Printf("Creating input container...")
	inputContainer := container.NewBorder(nil, nil, nil, gui.sendButton, gui.messageEntry)
	if inputContainer == nil {
		log.Fatal("Failed to create input container")
	}
	log.Printf("Input container created successfully")

	// User list container with header
	log.Printf("Creating user header...")
	userHeader := widget.NewCard("Users", "", gui.userList)
	if userHeader == nil {
		log.Fatal("Failed to create user header")
	}
	userHeader.Resize(fyne.NewSize(userListMinWidth, 0))
	log.Printf("User header created successfully")

	// Chat container
	log.Printf("Creating chat container...")
	chatContainer := container.NewBorder(nil, inputContainer, nil, nil, gui.chatScroll)
	if chatContainer == nil {
		log.Fatal("Failed to create chat container")
	}
	log.Printf("Chat container created successfully")

	// Main content
	log.Printf("Creating main content...")
	content := container.NewBorder(
		gui.statusLabel, // top
		nil,             // bottom
		userHeader,      // left
		nil,             // right
		chatContainer,   // center
	)
	if content == nil {
		log.Fatal("Failed to create main content")
	}
	log.Printf("Main content created successfully")

	// Menu
	log.Printf("Setting up menus...")
	gui.setupMenus()
	log.Printf("Menus setup complete")

	log.Printf("Setting window content...")
	gui.window.SetContent(content)
	log.Printf("Window content set successfully")

	log.Printf("Setting focus to message entry...")
	gui.messageEntry.FocusGained()
	log.Printf("UI setup complete")
}

// createMessageWidget creates a new message display widget with proper sizing
func (gui *MarchatGUI) createMessageWidget(sender, content string, timestamp time.Time, msgType shared.MessageType, file *shared.FileMeta) fyne.CanvasObject {
	// Create a container with proper padding and background
	messageBox := container.NewVBox()

	// Header with sender and time
	senderLabel := widget.NewLabel("")
	senderLabel.TextStyle = fyne.TextStyle{Bold: true}

	timeLabel := widget.NewLabel("")
	timeLabel.Importance = widget.LowImportance

	header := container.NewHBox(senderLabel, layout.NewSpacer(), timeLabel)

	// Content with proper wrapping
	contentLabel := widget.NewRichTextFromMarkdown("")
	contentLabel.Wrapping = fyne.TextWrapWord

	// Add components to main container
	messageBox.Add(header)
	messageBox.Add(contentLabel)

	// Add some visual separation with a separator
	separator := widget.NewSeparator()
	messageBox.Add(separator)

	return messageBox
}

// createScrollMessageWidget creates a message widget for the scroll container (simpler, more reliable)
func (gui *MarchatGUI) createScrollMessageWidget(sender, content string, timestamp time.Time, msgType shared.MessageType, file *shared.FileMeta) fyne.CanvasObject {
	// Create a card for each message
	senderText := sender
	if gui.cfg != nil && sender == gui.cfg.Username {
		senderText = sender + " (me)"
	}

	timeFmt := "15:04:05"
	if !gui.twentyFourHour {
		timeFmt = "03:04:05 PM"
	}
	timeText := timestamp.Format(timeFmt)

	// Create header with smaller font size
	headerLabel := widget.NewLabel(fmt.Sprintf("%s - %s", senderText, timeText))
	headerLabel.TextStyle = fyne.TextStyle{Bold: true}
	headerLabel.Importance = widget.MediumImportance

	var processedContent string
	if msgType == shared.FileMessageType && file != nil {
		processedContent = fmt.Sprintf("📎 File: %s (%d bytes)\n\nUse File → Save Received File to save",
			file.Filename, file.Size)
	} else {
		processedContent = gui.processMessageContent(content, sender)
	}

	contentLabel := widget.NewRichTextFromMarkdown(processedContent)
	contentLabel.Wrapping = fyne.TextWrapWord

	// Create a container with header and content instead of using card title
	messageContainer := container.NewVBox(
		headerLabel,
		contentLabel,
	)

	messageCard := widget.NewCard("", "", messageContainer)
	messageCard.Resize(fyne.NewSize(0, messageCard.MinSize().Height))

	return messageCard
}

// updateMessageWidget updates a message widget with new data and proper sizing
func (gui *MarchatGUI) updateMessageWidget(obj fyne.CanvasObject, sender, content string, timestamp time.Time, msgType shared.MessageType, file *shared.FileMeta) {
	messageBox := obj.(*fyne.Container)

	header := messageBox.Objects[0].(*fyne.Container)
	senderLabel := header.Objects[0].(*widget.Label)
	timeLabel := header.Objects[2].(*widget.Label)
	contentWidget := messageBox.Objects[1].(*widget.RichText)

	// Update sender
	if gui.cfg != nil && sender == gui.cfg.Username {
		senderLabel.SetText(sender + " (me)")
		senderLabel.Importance = widget.MediumImportance
	} else {
		senderLabel.SetText(sender)
		senderLabel.Importance = widget.LowImportance
	}

	// Update timestamp
	timeFmt := "15:04:05"
	if !gui.twentyFourHour {
		timeFmt = "03:04:05 PM"
	}
	timeLabel.SetText(timestamp.Format(timeFmt))

	// Update content
	var processedContent string
	if msgType == shared.FileMessageType && file != nil {
		processedContent = fmt.Sprintf("📎 **File:** %s (%d bytes)\n\n*Use File → Save Received File to save*",
			file.Filename, file.Size)
	} else {
		processedContent = gui.processMessageContent(content, sender)
	}

	contentWidget.ParseMarkdown(processedContent)

	// Refresh all components
	senderLabel.Refresh()
	timeLabel.Refresh()
	contentWidget.Refresh()
	messageBox.Refresh()
}

// processMessageContent processes message content for mentions, URLs, code blocks, etc.
func (gui *MarchatGUI) processMessageContent(content, sender string) string {
	// Convert basic emojis
	emojis := map[string]string{
		":)": "😊",
		":(": "🙁",
		":D": "😃",
		"<3": "❤️",
		":P": "😛",
	}
	for k, v := range emojis {
		content = strings.ReplaceAll(content, k, v)
	}

	// Process code blocks (simplified for markdown)
	codeBlockRegex := regexp.MustCompile("```([a-zA-Z0-9+]*)\n([\\s\\S]*?)```")
	content = codeBlockRegex.ReplaceAllString(content, "```\n$2\n```")

	// Highlight mentions
	gui.mu.RLock()
	users := make([]string, len(gui.users))
	copy(users, gui.users)
	gui.mu.RUnlock()

	matches := mentionRegex.FindAllStringSubmatch(content, -1)
	for _, m := range matches {
		if len(m) > 1 {
			for _, user := range users {
				if strings.EqualFold(m[1], user) {
					// Bold the mention
					content = strings.ReplaceAll(content, m[0], "**"+m[0]+"**")
					break
				}
			}
		}
	}

	return content
}

// setupMenus creates the application menus
func (gui *MarchatGUI) setupMenus() {
	// File menu
	fileMenu := fyne.NewMenu("File",
		fyne.NewMenuItem("Send File", gui.showFilePickerDialog),
		fyne.NewMenuItem("Save Received File", gui.showSaveFileDialog),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Quit", func() { gui.window.Close() }),
	)

	// Edit menu
	editMenu := fyne.NewMenu("Edit",
		fyne.NewMenuItem("Clear Chat", gui.clearChat),
		fyne.NewMenuItem("Code Snippet", gui.showCodeSnippetDialog),
	)

	// View menu
	viewMenu := fyne.NewMenu("View",
		fyne.NewMenuItem("Toggle Time Format", gui.toggleTimeFormat),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Light Theme", func() { gui.setTheme("light") }),
		fyne.NewMenuItem("Dark Theme", func() { gui.setTheme("dark") }),
		fyne.NewMenuItem("System Theme", func() { gui.setTheme("system") }),
	)

	// Audio menu
	audioMenu := fyne.NewMenu("Audio",
		fyne.NewMenuItem("Toggle Bell", gui.toggleBell),
		fyne.NewMenuItem("Toggle Bell on Mention Only", gui.toggleBellOnMention),
	)

	// Admin menu (only if admin)
	var adminMenu *fyne.Menu
	if gui.isAdmin {
		adminMenu = fyne.NewMenu("Admin",
			fyne.NewMenuItem("Database Operations", gui.showAdminDialog),
			fyne.NewMenuItemSeparator(),
			fyne.NewMenuItem("Kick Selected User", func() { gui.executeAdminAction("kick") }),
			fyne.NewMenuItem("Ban Selected User", func() { gui.executeAdminAction("ban") }),
			fyne.NewMenuItem("Force Disconnect User", func() { gui.executeAdminAction("forcedisconnect") }),
			fyne.NewMenuItemSeparator(),
			fyne.NewMenuItem("Unban User", func() { gui.promptAdminAction("unban") }),
			fyne.NewMenuItem("Allow User", func() { gui.promptAdminAction("allow") }),
		)
	}

	// Help menu
	helpMenu := fyne.NewMenu("Help",
		fyne.NewMenuItem("Show Help", gui.showHelpDialog),
		fyne.NewMenuItem("About", gui.showAboutDialog),
	)

	// Create main menu
	var menus []*fyne.Menu
	menus = append(menus, fileMenu, editMenu, viewMenu, audioMenu)
	if adminMenu != nil {
		menus = append(menus, adminMenu)
	}
	menus = append(menus, helpMenu)

	mainMenu := fyne.NewMainMenu(menus...)
	gui.window.SetMainMenu(mainMenu)
}

// setupKeyboardShortcuts sets up keyboard shortcuts
func (gui *MarchatGUI) setupKeyboardShortcuts() {
	gui.window.Canvas().SetOnTypedKey(func(ev *fyne.KeyEvent) {
		if ev.Name == fyne.KeyReturn && fyne.CurrentDevice().IsMobile() == false {
			// Handle Ctrl+Enter for new line, Enter for send
			gui.sendMessage("")
		}
	})

	// Set up accelerators in menus for common actions
	// This is handled through the menu items and their callbacks
}

// Connect establishes WebSocket connection
func (gui *MarchatGUI) Connect() error {
	gui.updateStatus("Connecting...")

	if gui.cfg == nil {
		return fmt.Errorf("configuration is nil")
	}

	escapedUsername := url.QueryEscape(gui.cfg.Username)
	fullURL := gui.cfg.ServerURL + "?username=" + escapedUsername

	log.Printf("Attempting to connect to: %s", fullURL)

	dialer := websocket.DefaultDialer
	// Skip TLS verification by default for development
	dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	conn, resp, err := dialer.Dial(fullURL, nil)
	if err != nil {
		log.Printf("WebSocket dial failed: %v", err)

		if resp != nil && resp.StatusCode == 403 {
			return fmt.Errorf("Username already taken - please choose a different username")
		}
		return err
	}

	gui.conn = conn
	gui.connected = true
	gui.updateStatus("✅ Connected to server!")
	gui.ctx, gui.cancel = context.WithCancel(context.Background())

	// Send handshake
	handshake := shared.Handshake{
		Username: gui.cfg.Username,
		Admin:    gui.isAdmin,
		AdminKey: gui.adminKey,
	}

	if err := gui.conn.WriteJSON(handshake); err != nil {
		return err
	}

	// Start connection handlers
	gui.startPingHandler()
	gui.startMessageHandler()

	return nil
}

// startPingHandler starts the ping/pong handler
func (gui *MarchatGUI) startPingHandler() {
	gui.wg.Add(1)
	go func() {
		defer gui.wg.Done()
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()

		gui.conn.SetPongHandler(func(appData string) error {
			return nil
		})

		for {
			select {
			case <-gui.ctx.Done():
				return
			case <-ticker.C:
				if gui.conn != nil {
					_ = gui.conn.WriteMessage(websocket.PingMessage, nil)
				}
			}
		}
	}()
}

// startMessageHandler starts the message receiving handler
func (gui *MarchatGUI) startMessageHandler() {
	gui.wg.Add(1)
	go func() {
		defer gui.wg.Done()

		for {
			select {
			case <-gui.ctx.Done():
				return
			default:
				if gui.conn == nil {
					return
				}

				msgType, raw, err := gui.conn.ReadMessage()
				if err != nil {
					log.Printf("WebSocket read error: %v", err)
					gui.handleConnectionError(err)
					return
				}

				if msgType == websocket.CloseMessage {
					log.Printf("Received close message: %s", string(raw))
					gui.handleConnectionError(fmt.Errorf("connection closed: %s", string(raw)))
					return
				}

				gui.processIncomingMessage(raw)
			}
		}
	}()
}

// processIncomingMessage processes incoming WebSocket messages
func (gui *MarchatGUI) processIncomingMessage(raw []byte) {
	// Try to unmarshal as shared.Message first
	var msg shared.Message
	if err := json.Unmarshal(raw, &msg); err == nil && msg.Sender != "" {
		gui.handleIncomingMessage(msg)
		return
	}

	// Try as wsMsg for special messages
	type wsMsg struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}

	var ws wsMsg
	if err := json.Unmarshal(raw, &ws); err == nil && ws.Type != "" {
		gui.handleSpecialMessage(ws)
		return
	}

	log.Printf("Could not parse message: %s", string(raw))
}

// handleIncomingMessage handles regular chat messages
func (gui *MarchatGUI) handleIncomingMessage(msg shared.Message) {
	// Decrypt if encrypted
	if gui.keystore != nil && msg.Content != "" {
		log.Printf("Attempting to decrypt message from %s", msg.Sender)
		log.Printf("Keystore instance: %p", gui.keystore)
		if decoded, err := base64.StdEncoding.DecodeString(msg.Content); err == nil && len(decoded) > 12 {
			nonce := decoded[:12]
			encryptedData := decoded[12:]
			log.Printf("Message appears to be encrypted (length: %d)", len(decoded))

			encryptedMsg := shared.EncryptedMessage{
				Sender:      msg.Sender,
				CreatedAt:   msg.CreatedAt,
				Encrypted:   encryptedData,
				Nonce:       nonce,
				IsEncrypted: true,
				Type:        msg.Type,
			}

			// Check if we have a global key
			globalKey := gui.keystore.GetSessionKey("global")

			if globalKey == nil {
				log.Printf("No global key available for decryption")
				log.Printf("Keystore instance: %p, checking if it's properly initialized", gui.keystore)
				// Check environment variable in decryption context
				envKey := os.Getenv("MARCHAT_GLOBAL_E2E_KEY")
				if envKey != "" {
					log.Printf("Environment variable is set in decryption context (length: %d)", len(envKey))
				} else {
					log.Printf("Environment variable is NOT set in decryption context!")
				}
				msg.Content = "[ENCRYPTED - NO GLOBAL KEY]"
			} else {
				log.Printf("Global key available: KeyID=%s", globalKey.KeyID)
				if decryptedMsg, err := gui.keystore.DecryptMessage(&encryptedMsg, "global"); err == nil {
					log.Printf("Successfully decrypted message from %s", msg.Sender)
					msg = *decryptedMsg
				} else {
					log.Printf("Failed to decrypt message from %s: %v", msg.Sender, err)
					msg.Content = "[ENCRYPTED - DECRYPTION FAILED]"
				}
			}
		} else {
			log.Printf("Message from %s is not encrypted or too short", msg.Sender)
		}
	} else {
		log.Printf("No keystore available or empty message content")
	}

	// Check for bell notification
	if gui.shouldPlayBell(msg) {
		gui.playBell()
	}

	gui.mu.Lock()
	// Limit message history
	if len(gui.messages) >= maxMessages {
		gui.messages = gui.messages[len(gui.messages)-maxMessages+1:]
	}
	gui.messages = append(gui.messages, msg)

	// Handle file messages
	if msg.Type == shared.FileMessageType && msg.File != nil {
		gui.receivedFiles[msg.File.Filename] = msg.File
	}

	// Sort messages to maintain order
	gui.sortMessages()
	gui.mu.Unlock()

	// Update UI on main thread
	fyne.Do(func() {
		// Add message to scroll container
		messageWidget := gui.createScrollMessageWidget(msg.Sender, msg.Content, msg.CreatedAt, msg.Type, msg.File)
		gui.messageContainer.Add(messageWidget)

		// Limit messages
		if len(gui.messageContainer.Objects) > maxMessages {
			gui.messageContainer.Remove(gui.messageContainer.Objects[0])
		}

		// Refresh and scroll to bottom
		gui.messageContainer.Refresh()

		// Scroll to bottom to show newest message
		if gui.chatScroll != nil {
			gui.chatScroll.ScrollToBottom()
		}
	})

	gui.setSending(false)
}

// handleSpecialMessage handles special WebSocket messages
func (gui *MarchatGUI) handleSpecialMessage(ws struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}) {
	switch ws.Type {
	case "userlist":
		type UserList struct {
			Users []string `json:"users"`
		}
		var ul UserList
		if err := json.Unmarshal(ws.Data, &ul); err == nil {
			gui.mu.Lock()
			gui.users = ul.Users
			gui.mu.Unlock()
			fyne.Do(func() {
				if gui.userList != nil {
					gui.userList.Refresh()
				}
			})
		}
	case "auth_failed":
		var authFail map[string]string
		if err := json.Unmarshal(ws.Data, &authFail); err == nil {
			fyne.Do(func() {
				gui.showErrorDialog("Authentication Failed", authFail["reason"])
			})
		}
	}
}

// handleConnectionError handles connection errors and reconnection
func (gui *MarchatGUI) handleConnectionError(err error) {
	gui.connected = false
	gui.updateStatus("🚫 Connection lost. Reconnecting...")
	gui.closeConnection()

	// Check for username errors
	if strings.Contains(err.Error(), "Username already taken") ||
		strings.Contains(err.Error(), "already taken") {
		gui.updateStatus("❌ Username already taken - please restart with a different username")
		return
	}

	// Attempt reconnection with exponential backoff
	delay := gui.reconnectDelay
	if delay < reconnectMaxDelay {
		gui.reconnectDelay *= 2
		if gui.reconnectDelay > reconnectMaxDelay {
			gui.reconnectDelay = reconnectMaxDelay
		}
	}

	time.AfterFunc(delay, func() {
		if err := gui.Connect(); err != nil {
			log.Printf("Reconnection failed: %v", err)
		} else {
			gui.reconnectDelay = time.Second // Reset on success
		}
	})
}

// sortMessages sorts messages by timestamp
func (gui *MarchatGUI) sortMessages() {
	sort.Slice(gui.messages, func(i, j int) bool {
		if !gui.messages[i].CreatedAt.Equal(gui.messages[j].CreatedAt) {
			return gui.messages[i].CreatedAt.Before(gui.messages[j].CreatedAt)
		}
		if gui.messages[i].Sender != gui.messages[j].Sender {
			return gui.messages[i].Sender < gui.messages[j].Sender
		}
		return gui.messages[i].Content < gui.messages[j].Content
	})
}

// shouldPlayBell determines if a bell should be played
func (gui *MarchatGUI) shouldPlayBell(msg shared.Message) bool {
	if (gui.cfg != nil && msg.Sender == gui.cfg.Username) || !gui.bellEnabled {
		return false
	}

	if gui.bellOnMention {
		username := "user"
		if gui.cfg != nil {
			username = gui.cfg.Username
		}
		mentionPattern := fmt.Sprintf("@%s", username)
		return strings.Contains(strings.ToLower(msg.Content), strings.ToLower(mentionPattern))
	}

	return true
}

// playBell plays a bell notification with rate limiting
func (gui *MarchatGUI) playBell() {
	now := time.Now()
	if now.Sub(gui.lastBellTime) < gui.minBellInterval {
		return
	}
	gui.lastBellTime = now

	// Try to play system notification sound
	go func() {
		switch runtime.GOOS {
		case "darwin":
			exec.Command("osascript", "-e", "beep").Run()
		case "windows":
			exec.Command("powershell", "-c", "[console]::beep(800,200)").Run()
		case "linux":
			exec.Command("paplay", "/usr/share/sounds/alsa/Front_Left.wav").Run()
		default:
			fmt.Print("\a") // Fallback to ASCII bell
		}
	}()
}

// sendMessage sends a message
func (gui *MarchatGUI) sendMessage(text string) {
	if text == "" {
		text = gui.messageEntry.Text
	}

	if text == "" {
		return
	}

	// Handle commands
	if gui.handleCommand(text) {
		gui.messageEntry.SetText("")
		return
	}

	if !gui.connected || gui.conn == nil {
		gui.updateStatus("❌ Not connected")
		return
	}

	gui.setSending(true)

	if gui.keystore != nil {
		gui.sendEncryptedMessage(text)
	} else {
		gui.sendPlainMessage(text)
	}

	gui.messageEntry.SetText("")
}

// handleCommand processes chat commands
func (gui *MarchatGUI) handleCommand(text string) bool {
	switch {
	case text == ":clear":
		gui.clearChat()
		return true
	case text == ":time":
		gui.toggleTimeFormat()
		return true
	case text == ":bell":
		gui.toggleBell()
		return true
	case text == ":bell-mention":
		gui.toggleBellOnMention()
		return true
	case text == ":code":
		gui.showCodeSnippetDialog()
		return true
	case text == ":sendfile":
		gui.showFilePickerDialog()
		return true
	case strings.HasPrefix(text, ":sendfile "):
		parts := strings.SplitN(text, " ", 2)
		if len(parts) == 2 {
			gui.sendFile(strings.TrimSpace(parts[1]))
		}
		return true
	case strings.HasPrefix(text, ":savefile "):
		filename := strings.TrimSpace(strings.TrimPrefix(text, ":savefile "))
		gui.saveFile(filename)
		return true
	case strings.HasPrefix(text, ":theme "):
		parts := strings.SplitN(text, " ", 2)
		if len(parts) == 2 {
			gui.setTheme(strings.TrimSpace(parts[1]))
		}
		return true
	case gui.isAdmin && gui.isAdminCommand(text):
		gui.sendAdminCommand(text)
		return true
	}

	return false
}

// isAdminCommand checks if text is an admin command
func (gui *MarchatGUI) isAdminCommand(text string) bool {
	adminCommands := []string{":cleardb", ":backup", ":stats", ":kick", ":ban", ":unban", ":allow", ":forcedisconnect"}
	for _, cmd := range adminCommands {
		if text == cmd || strings.HasPrefix(text, cmd+" ") {
			return true
		}
	}
	return false
}

// sendPlainMessage sends an unencrypted message
func (gui *MarchatGUI) sendPlainMessage(text string) {
	username := "user"
	if gui.cfg != nil {
		username = gui.cfg.Username
	}
	msg := shared.Message{
		Sender:    username,
		Content:   text,
		CreatedAt: time.Now(),
	}

	if err := gui.conn.WriteJSON(msg); err != nil {
		gui.updateStatus("❌ Failed to send message")
		log.Printf("Failed to send message: %v", err)
	}
	gui.setSending(false)
}

// sendEncryptedMessage sends an encrypted message
func (gui *MarchatGUI) sendEncryptedMessage(text string) {
	if gui.keystore == nil {
		log.Printf("No keystore available for encryption")
		gui.updateStatus("❌ Encryption not available")
		gui.setSending(false)
		return
	}

	log.Printf("Encrypting message: %s", text)
	// Check if global key is available
	globalKey := gui.keystore.GetSessionKey("global")
	if globalKey == nil {
		log.Printf("No global key available for encryption")
		gui.updateStatus("❌ No global key available for encryption")
		gui.setSending(false)
		return
	}
	log.Printf("Global key available for encryption: KeyID=%s", globalKey.KeyID)

	// Encrypt using global key
	conversationID := "global"
	username := "user"
	if gui.cfg != nil {
		username = gui.cfg.Username
	}
	encryptedMsg, err := gui.keystore.EncryptMessage(username, text, conversationID)
	if err != nil {
		log.Printf("Encryption failed: %v", err)
		gui.updateStatus(fmt.Sprintf("❌ Encryption failed: %v", err))
		gui.setSending(false)
		return
	}
	log.Printf("Message encrypted successfully")

	// Combine nonce + encrypted data and base64 encode
	combinedData := make([]byte, 0, len(encryptedMsg.Nonce)+len(encryptedMsg.Encrypted))
	combinedData = append(combinedData, encryptedMsg.Nonce...)
	combinedData = append(combinedData, encryptedMsg.Encrypted...)

	finalContent := base64.StdEncoding.EncodeToString(combinedData)

	// username is already declared above, just reassign if needed
	if gui.cfg != nil {
		username = gui.cfg.Username
	}
	msg := shared.Message{
		Content:   finalContent,
		Sender:    username,
		CreatedAt: time.Now(),
		Type:      shared.TextMessage,
	}

	if err := gui.conn.WriteJSON(msg); err != nil {
		gui.updateStatus("❌ Failed to send encrypted message")
		log.Printf("Failed to send encrypted message: %v", err)
	}
	gui.setSending(false)
}

// sendAdminCommand sends admin commands
func (gui *MarchatGUI) sendAdminCommand(text string) {
	if !gui.isAdmin {
		gui.updateStatus("❌ Admin privileges required")
		return
	}

	username := "user"
	if gui.cfg != nil {
		username = gui.cfg.Username
	}
	msg := shared.Message{
		Sender:  username,
		Content: text,
		Type:    shared.TextMessage, // Use TextMessage for admin commands
	}

	if err := gui.conn.WriteJSON(msg); err != nil {
		gui.updateStatus("❌ Failed to send admin command")
	} else {
		gui.updateStatus("✅ Admin command sent")
	}
}

// sendFile sends a file
func (gui *MarchatGUI) sendFile(filePath string) {
	if !gui.connected || gui.conn == nil {
		gui.updateStatus("❌ Not connected")
		return
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		gui.updateStatus("❌ Failed to read file: " + err.Error())
		return
	}

	// Check file size limit
	var maxBytes int64 = 1024 * 1024 // Default 1MB
	if envBytes := os.Getenv("MARCHAT_MAX_FILE_BYTES"); envBytes != "" {
		if v, err := strconv.ParseInt(envBytes, 10, 64); err == nil && v > 0 {
			maxBytes = v
		}
	} else if envMB := os.Getenv("MARCHAT_MAX_FILE_MB"); envMB != "" {
		if v, err := strconv.ParseInt(envMB, 10, 64); err == nil && v > 0 {
			maxBytes = v * 1024 * 1024
		}
	}

	if int64(len(data)) > maxBytes {
		limitMsg := fmt.Sprintf("%d bytes", maxBytes)
		if maxBytes%(1024*1024) == 0 {
			limitMsg = fmt.Sprintf("%dMB", maxBytes/(1024*1024))
		}
		gui.updateStatus("❌ File too large (max " + limitMsg + ")")
		return
	}

	filename := filepath.Base(filePath)
	username := "user"
	if gui.cfg != nil {
		username = gui.cfg.Username
	}
	msg := shared.Message{
		Sender:    username,
		Type:      shared.FileMessageType,
		CreatedAt: time.Now(),
		File: &shared.FileMeta{
			Filename: filename,
			Size:     int64(len(data)),
			Data:     data,
		},
	}

	if err := gui.conn.WriteJSON(msg); err != nil {
		gui.updateStatus("❌ Failed to send file")
	} else {
		gui.updateStatus("✅ File sent: " + filename)
	}
}

// saveFile saves a received file
func (gui *MarchatGUI) saveFile(filename string) {
	gui.mu.RLock()
	file, exists := gui.receivedFiles[filename]
	gui.mu.RUnlock()

	if !exists {
		gui.updateStatus("❌ File not found: " + filename)
		return
	}

	// Show save dialog
	saveDialog := dialog.NewFileSave(
		func(writer fyne.URIWriteCloser, err error) {
			if err != nil || writer == nil {
				return
			}
			defer writer.Close()

			if _, err := writer.Write(file.Data); err != nil {
				gui.updateStatus("❌ Failed to save file: " + err.Error())
			} else {
				gui.updateStatus("✅ File saved: " + writer.URI().Name())
			}
		},
		gui.window,
	)

	saveDialog.SetFileName(filename)
	saveDialog.Show()
}

// UI Helper Methods

// updateStatus updates the status label
func (gui *MarchatGUI) updateStatus(status string) {
	fyne.Do(func() {
		if gui.statusLabel != nil {
			gui.statusLabel.SetText(status)
			gui.statusLabel.Refresh()
		}
	})
}

// setSending updates the sending state
func (gui *MarchatGUI) setSending(sending bool) {
	gui.mu.Lock()
	gui.sending = sending
	gui.mu.Unlock()

	fyne.Do(func() {
		if gui.sendButton != nil {
			gui.sendButton.SetText(map[bool]string{true: "Sending...", false: "Send"}[sending])
			gui.sendButton.Refresh()
		}
	})
}

// closeConnection closes the WebSocket connection
func (gui *MarchatGUI) closeConnection() {
	if gui.cancel != nil {
		gui.cancel()
	}
	if gui.conn != nil {
		gui.conn.Close()
		gui.conn = nil
	}
	gui.wg.Wait()
}

// scrollToBottom scrolls the chat to show the newest messages
func (gui *MarchatGUI) scrollToBottom() {
	if gui.chatScroll != nil {
		gui.chatScroll.ScrollToBottom()
	}
}

// Action Methods

// clearChat clears the chat history
func (gui *MarchatGUI) clearChat() {
	gui.mu.Lock()
	gui.messages = nil
	gui.mu.Unlock()

	fyne.Do(func() {
		// Clear the scroll container
		gui.messageContainer.RemoveAll()
		gui.messageContainer.Refresh()
	})
	gui.updateStatus("Chat cleared")
}

// toggleTimeFormat toggles between 12/24 hour format
func (gui *MarchatGUI) toggleTimeFormat() {
	gui.twentyFourHour = !gui.twentyFourHour
	if gui.cfg != nil {
		gui.cfg.TwentyFourHour = gui.twentyFourHour
	}
	gui.saveConfig()

	status := "12h"
	if gui.twentyFourHour {
		status = "24h"
	}
	gui.updateStatus("Time format: " + status)

	fyne.Do(func() {
		// Recreate all messages with new time format
		gui.mu.RLock()
		messages := make([]shared.Message, len(gui.messages))
		copy(messages, gui.messages)
		gui.mu.RUnlock()

		// Clear and rebuild the container
		gui.messageContainer.RemoveAll()
		for _, msg := range messages {
			messageWidget := gui.createScrollMessageWidget(msg.Sender, msg.Content, msg.CreatedAt, msg.Type, msg.File)
			gui.messageContainer.Add(messageWidget)
		}
		gui.messageContainer.Refresh()

		// Scroll to bottom after rebuilding
		if gui.chatScroll != nil {
			gui.chatScroll.ScrollToBottom()
		}
	})
}

// toggleBell toggles bell notifications
func (gui *MarchatGUI) toggleBell() {
	gui.bellEnabled = !gui.bellEnabled
	// Note: EnableBell field doesn't exist in config, just update local state
	gui.saveConfig()

	status := "disabled"
	if gui.bellEnabled {
		status = "enabled"
		gui.playBell() // Test beep
	}
	gui.updateStatus("Bell notifications " + status)
}

// toggleBellOnMention toggles bell on mention only
func (gui *MarchatGUI) toggleBellOnMention() {
	gui.bellOnMention = !gui.bellOnMention
	// Note: BellOnMention field doesn't exist in config, just update local state
	gui.saveConfig()

	status := "disabled"
	if gui.bellOnMention {
		status = "enabled"
		if gui.bellEnabled {
			gui.playBell() // Test beep
		}
	}
	gui.updateStatus("Bell on mention only " + status)
}

// setTheme changes the application theme
func (gui *MarchatGUI) setTheme(themeName string) {
	if gui.cfg != nil {
		gui.cfg.Theme = themeName
	}
	gui.app.Settings().SetTheme(getThemeFromConfig(themeName))
	gui.saveConfig()
	gui.updateStatus("Theme changed to " + themeName)
}

// saveConfig saves the current configuration
func (gui *MarchatGUI) saveConfig() {
	// Simplified config saving - just log for now
	log.Printf("Config would be saved here")
}

// Admin Methods

// executeAdminAction executes admin actions on selected users
func (gui *MarchatGUI) executeAdminAction(action string) {
	if !gui.isAdmin {
		gui.updateStatus("❌ Admin privileges required")
		return
	}

	gui.mu.RLock()
	selectedUser := gui.selectedUser
	gui.mu.RUnlock()

	if selectedUser == "" {
		gui.updateStatus("❌ No user selected")
		return
	}

	if gui.cfg != nil && selectedUser == gui.cfg.Username {
		gui.updateStatus("❌ Cannot perform action on yourself")
		return
	}

	command := fmt.Sprintf(":%s %s", action, selectedUser)
	gui.sendAdminCommand(command)

	// Clear selection after kick/ban/disconnect
	if action == "kick" || action == "ban" || action == "forcedisconnect" {
		gui.mu.Lock()
		gui.selectedUserIndex = -1
		gui.selectedUser = ""
		gui.mu.Unlock()
		fyne.Do(func() {
			if gui.userList != nil {
				gui.userList.Refresh()
			}
		})
	}
}

// promptAdminAction prompts for username for admin actions
func (gui *MarchatGUI) promptAdminAction(action string) {
	if !gui.isAdmin {
		gui.updateStatus("❌ Admin privileges required")
		return
	}

	usernameEntry := widget.NewEntry()
	usernameEntry.SetPlaceHolder("Enter username...")

	content := container.NewVBox(
		widget.NewLabel(fmt.Sprintf("Enter username to %s:", action)),
		usernameEntry,
	)

	dialog := dialog.NewCustomConfirm(
		strings.Title(action)+" User",
		"Execute",
		"Cancel",
		content,
		func(confirmed bool) {
			if confirmed && usernameEntry.Text != "" {
				command := fmt.Sprintf(":%s %s", action, usernameEntry.Text)
				gui.sendAdminCommand(command)
			}
		},
		gui.window,
	)

	dialog.Show()
	usernameEntry.FocusGained()
}

// Dialog Methods

// showHelpDialog shows the help dialog
func (gui *MarchatGUI) showHelpDialog() {
	helpText := gui.generateHelpText()

	helpLabel := widget.NewRichTextFromMarkdown(helpText)
	helpLabel.Wrapping = fyne.TextWrapWord

	helpScroll := container.NewScroll(helpLabel)
	helpScroll.SetMinSize(fyne.NewSize(600, 400))

	dialog := dialog.NewCustom(
		"marchat Help",
		"Close",
		helpScroll,
		gui.window,
	)

	dialog.Resize(fyne.NewSize(700, 500))
	dialog.Show()
}

// generateHelpText generates help content
func (gui *MarchatGUI) generateHelpText() string {
	help := `# marchat Help

## Keyboard Shortcuts
- **Enter**: Send message
- **Ctrl+Enter**: New line in message

## Menu Commands
### File Menu
- **Send File**: Send a file to the chat
- **Save Received File**: Save a file that was sent to you

### Edit Menu  
- **Clear Chat**: Clear the chat history
- **Code Snippet**: Create a syntax highlighted code snippet

### View Menu
- **Toggle Time Format**: Switch between 12h and 24h time display
- **Themes**: Change between Light, Dark, and System themes

### Audio Menu
- **Toggle Bell**: Enable/disable notification sounds
- **Toggle Bell on Mention Only**: Only play sound when mentioned

## Chat Commands
- ` + "`" + `:clear` + "`" + ` - Clear chat history
- ` + "`" + `:time` + "`" + ` - Toggle 12/24h time format  
- ` + "`" + `:bell` + "`" + ` - Toggle notification bell
- ` + "`" + `:bell-mention` + "`" + ` - Toggle bell only on mentions
- ` + "`" + `:code` + "`" + ` - Create code snippet
- ` + "`" + `:sendfile [path]` + "`" + ` - Send a file
- ` + "`" + `:savefile <filename>` + "`" + ` - Save received file
- ` + "`" + `:theme <name>` + "`" + ` - Change theme (light, dark, system)
`

	if gui.isAdmin {
		help += `
### Admin Menu (Admin Only)
- **Database Operations**: Access database management
- **User Actions**: Kick, ban, or disconnect users
- **Unban/Allow User**: Restore user access

## Admin Commands  
- ` + "`" + `:cleardb` + "`" + ` - Clear database
- ` + "`" + `:backup` + "`" + ` - Backup database
- ` + "`" + `:stats` + "`" + ` - Show database stats
- ` + "`" + `:kick <user>` + "`" + ` - Kick user
- ` + "`" + `:ban <user>` + "`" + ` - Ban user  
- ` + "`" + `:unban <user>` + "`" + ` - Unban user
- ` + "`" + `:allow <user>` + "`" + ` - Allow user (override kick)
- ` + "`" + `:forcedisconnect <user>` + "`" + ` - Force disconnect user
`
	}

	return help
}

// showAboutDialog shows the about dialog
func (gui *MarchatGUI) showAboutDialog() {
	aboutText := fmt.Sprintf(`# marchat GUI Client

**Version**: %s  
**Build**: Fyne GUI Edition

A secure, real-time chat client with end-to-end encryption support.

## Features
- Real-time messaging
- File sharing
- End-to-end encryption
- Multiple themes
- Admin capabilities
- Cross-platform support

Built with Go and Fyne.
`, "1.0.0") // Use hardcoded version for now

	aboutLabel := widget.NewRichTextFromMarkdown(aboutText)
	aboutLabel.Wrapping = fyne.TextWrapWord

	dialog := dialog.NewCustom(
		"About marchat",
		"Close",
		container.NewScroll(aboutLabel),
		gui.window,
	)

	dialog.Resize(fyne.NewSize(400, 300))
	dialog.Show()
}

// showCodeSnippetDialog shows the code snippet creation dialog
func (gui *MarchatGUI) showCodeSnippetDialog() {
	languageEntry := widget.NewEntry()
	languageEntry.SetPlaceHolder("Language (e.g., go, python, javascript)")

	codeEntry := widget.NewMultiLineEntry()
	codeEntry.SetPlaceHolder("Enter your code here...")
	codeEntry.Resize(fyne.NewSize(0, 200))

	content := container.NewVBox(
		widget.NewLabel("Create Code Snippet:"),
		widget.NewForm(
			widget.NewFormItem("Language:", languageEntry),
		),
		widget.NewLabel("Code:"),
		codeEntry,
	)

	dialog := dialog.NewCustomConfirm(
		"Code Snippet",
		"Send",
		"Cancel",
		content,
		func(confirmed bool) {
			if confirmed && codeEntry.Text != "" {
				language := languageEntry.Text
				code := codeEntry.Text

				// Format as markdown code block
				formattedCode := fmt.Sprintf("```%s\n%s\n```", language, code)
				gui.sendMessage(formattedCode)
			}
		},
		gui.window,
	)

	dialog.Resize(fyne.NewSize(600, 400))
	dialog.Show()
	languageEntry.FocusGained()
}

// showFilePickerDialog shows the file picker dialog
func (gui *MarchatGUI) showFilePickerDialog() {
	fileDialog := dialog.NewFileOpen(
		func(reader fyne.URIReadCloser, err error) {
			if err != nil || reader == nil {
				return
			}
			defer reader.Close()

			filePath := reader.URI().Path()
			gui.sendFile(filePath)
		},
		gui.window,
	)

	fileDialog.Show()
}

// showSaveFileDialog shows dialog to save received files
func (gui *MarchatGUI) showSaveFileDialog() {
	gui.mu.RLock()
	fileCount := len(gui.receivedFiles)
	var fileNames []string
	for name := range gui.receivedFiles {
		fileNames = append(fileNames, name)
	}
	gui.mu.RUnlock()

	if fileCount == 0 {
		dialog.ShowInformation("No Files", "No files have been received yet.", gui.window)
		return
	}

	// Sort file names
	sort.Strings(fileNames)

	fileList := widget.NewList(
		func() int { return len(fileNames) },
		func() fyne.CanvasObject {
			return widget.NewLabel("")
		},
		func(i widget.ListItemID, obj fyne.CanvasObject) {
			if i < len(fileNames) {
				label := obj.(*widget.Label)
				label.SetText(fileNames[i])
			}
		},
	)

	fileList.OnSelected = func(id widget.ListItemID) {
		if id < len(fileNames) {
			filename := fileNames[id]
			gui.saveFile(filename)
		}
	}

	content := container.NewVBox(
		widget.NewLabel("Select file to save:"),
		container.NewScroll(fileList),
	)

	dialog := dialog.NewCustom(
		"Save Received Files",
		"Close",
		content,
		gui.window,
	)

	dialog.Resize(fyne.NewSize(400, 300))
	dialog.Show()
}

// showAdminDialog shows the admin database operations dialog
func (gui *MarchatGUI) showAdminDialog() {
	if !gui.isAdmin {
		gui.updateStatus("❌ Admin privileges required")
		return
	}

	clearButton := widget.NewButton("Clear Database", func() {
		gui.confirmAdminAction("Clear Database", "This will delete all messages. Continue?", ":cleardb")
	})
	clearButton.Importance = widget.DangerImportance

	backupButton := widget.NewButton("Backup Database", func() {
		gui.sendAdminCommand(":backup")
		gui.updateStatus("✅ Database backup initiated")
	})

	statsButton := widget.NewButton("Show Statistics", func() {
		gui.sendAdminCommand(":stats")
		gui.updateStatus("✅ Database stats requested")
	})

	content := container.NewVBox(
		widget.NewLabel("Database Operations:"),
		clearButton,
		backupButton,
		statsButton,
	)

	dialog := dialog.NewCustom(
		"Admin - Database Operations",
		"Close",
		content,
		gui.window,
	)

	dialog.Show()
}

// confirmAdminAction shows confirmation dialog for dangerous admin actions
func (gui *MarchatGUI) confirmAdminAction(title, message, command string) {
	dialog := dialog.NewConfirm(
		title,
		message,
		func(confirmed bool) {
			if confirmed {
				gui.sendAdminCommand(command)
			}
		},
		gui.window,
	)

	dialog.Show()
}

// showErrorDialog shows an error dialog
func (gui *MarchatGUI) showErrorDialog(title, message string) {
	dialog.ShowError(fmt.Errorf(message), gui.window)
}

// Run starts the GUI application
func (gui *MarchatGUI) Run() {
	log.Printf("Starting GUI application...")

	// Add panic recovery
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC RECOVERED in GUI.Run(): %v", r)
			log.Printf("Stack trace: %s", string(debug.Stack()))
			// Try to show error dialog
			if gui.window != nil {
				gui.showErrorDialog("Application Error", fmt.Sprintf("A panic occurred: %v", r))
			}
		}
	}()

	// Set up window close handler
	log.Printf("Setting up window close handler...")
	gui.window.SetCloseIntercept(func() {
		log.Printf("Window close intercepted")
		gui.closeConnection()
		gui.app.Quit()
	})
	log.Printf("Window close handler set")

	// Connect to server
	log.Printf("Starting connection goroutine...")
	go func() {
		log.Printf("Attempting to connect to server...")
		if err := gui.Connect(); err != nil {
			log.Printf("Connection error: %v", err)
			fyne.Do(func() {
				gui.showErrorDialog("Connection Error", err.Error())
			})
		}
	}()
	log.Printf("Connection goroutine started")

	// Window is already shown, just log completion
	log.Printf("GUI setup complete - window already visible")
}

// Main function - entry point for GUI version
func main() {
	// Add panic recovery at the top level
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC RECOVERED in main(): %v", r)
			log.Printf("Stack trace: %s", string(debug.Stack()))
			fmt.Printf("FATAL ERROR: %v\n", r)
			os.Exit(1)
		}
	}()

	log.Printf("Starting marchat GUI application...")

	// For this example, we'll use a basic configuration
	// In a real implementation, you'd want to integrate with the existing config system

	var cfg *config.Config
	var keystore *crypto.KeyStore
	var isAdmin bool

	// Check command line arguments or show config dialog
	if len(os.Args) > 1 {
		// Handle command line configuration
		// This is simplified - integrate with existing config loading
		cfg = &config.Config{
			Username:  "TestUser",
			ServerURL: "ws://localhost:8080/ws",
			Theme:     "system",
		}
	} else {
		// Create the main Fyne app first
		log.Printf("Creating main Fyne app...")
		fyneApp := app.NewWithID("com.marchat.client")
		if fyneApp == nil {
			log.Fatal("Failed to create main Fyne app")
		}

		// Show initial configuration dialog and get the GUI ready to run
		log.Printf("Showing configuration dialog...")
		gui := showConfigDialog(fyneApp)

		// Handle system signals
		log.Printf("Setting up signal handlers...")
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-c
			log.Printf("Received termination signal")
			if gui != nil {
				gui.closeConnection()
			}
			fyneApp.Quit()
		}()
		log.Printf("Signal handlers set up")

		// Run the app - this will show the config dialog first
		// When connect is clicked, the content will switch to chat UI
		log.Printf("Starting app run loop...")
		fyneApp.Run()
		log.Printf("App run loop completed")
		return
	}

	// This path is for command line args (simplified)
	// Initialize keystore if E2E is enabled (simplified for now)
	// For now, we'll skip E2E initialization to avoid dependency issues
	keystore = nil

	// Create and run GUI
	log.Printf("Creating GUI instance...")
	gui := NewMarchatGUI(cfg, keystore, isAdmin, "")

	if gui == nil {
		log.Fatal("Failed to create GUI")
	}
	log.Printf("GUI instance created successfully")

	// Handle system signals
	log.Printf("Setting up signal handlers...")
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		log.Printf("Received termination signal")
		gui.closeConnection()
		gui.app.Quit()
	}()
	log.Printf("Signal handlers set up")

	log.Printf("Starting GUI run loop...")
	gui.Run()
	log.Printf("GUI run loop completed")
}

// showConfigDialog shows initial configuration dialog and returns the GUI ready to run
func showConfigDialog(fyneApp fyne.App) *MarchatGUI {
	// Create the main window that will be reused
	window := fyneApp.NewWindow("marchat - Configuration")
	window.Resize(fyne.NewSize(500, 400))
	window.CenterOnScreen()

	// Form fields
	usernameEntry := widget.NewEntry()
	usernameEntry.SetPlaceHolder("Enter your username")

	serverEntry := widget.NewEntry()
	serverEntry.SetText("ws://localhost:8080/ws")

	adminCheck := widget.NewCheck("Connect as admin", nil)
	adminKeyEntry := widget.NewPasswordEntry()
	adminKeyEntry.SetPlaceHolder("Admin key (if admin)")

	e2eCheck := widget.NewCheck("Enable end-to-end encryption", nil)
	e2ePassEntry := widget.NewPasswordEntry()
	e2ePassEntry.SetPlaceHolder("Keystore passphrase (if E2E)")

	globalE2EKeyEntry := widget.NewPasswordEntry()
	globalE2EKeyEntry.SetPlaceHolder("Global E2E key (MARCHAT_GLOBAL_E2E_KEY)")

	themeSelect := widget.NewSelect([]string{"system", "light", "dark"}, nil)
	themeSelect.SetSelected("system")

	// Form
	form := widget.NewForm(
		widget.NewFormItem("Username:", usernameEntry),
		widget.NewFormItem("Server URL:", serverEntry),
		widget.NewFormItem("", adminCheck),
		widget.NewFormItem("Admin Key:", adminKeyEntry),
		widget.NewFormItem("", e2eCheck),
		widget.NewFormItem("E2E Passphrase:", e2ePassEntry),
		widget.NewFormItem("Global E2E Key:", globalE2EKeyEntry),
		widget.NewFormItem("Theme:", themeSelect),
	)

	// Connect button that will create the GUI and switch content
	connectButton := widget.NewButton("Connect", func() {
		if usernameEntry.Text == "" {
			dialog.ShowError(fmt.Errorf("Username is required"), window)
			return
		}

		// Create config
		cfg := &config.Config{
			Username:       usernameEntry.Text,
			ServerURL:      serverEntry.Text,
			Theme:          themeSelect.Selected,
			TwentyFourHour: true,
		}

		// Set admin and E2E flags
		isAdmin := adminCheck.Checked
		var keystore *crypto.KeyStore

		// Set the global E2E key environment variable FIRST, before any keystore operations
		if e2eCheck.Checked && globalE2EKeyEntry.Text != "" {
			log.Printf("Setting MARCHAT_GLOBAL_E2E_KEY environment variable BEFORE any keystore operations...")
			os.Setenv("MARCHAT_GLOBAL_E2E_KEY", globalE2EKeyEntry.Text)
			log.Printf("Environment variable set to: %s", globalE2EKeyEntry.Text)
		} else if e2eCheck.Checked {
			log.Printf("E2E enabled but no global E2E key provided")
		}

		if e2eCheck.Checked {
			log.Printf("E2E enabled - will use existing keystore file")

			// Initialize keystore with passphrase
			if e2ePassEntry.Text != "" {
				log.Printf("Initializing keystore with passphrase...")
				// Use the same keystore path as the TUI client
				keystorePath := getKeystorePath()
				log.Printf("Using keystore path: %s", keystorePath)

				// Also check if there are other possible keystore locations
				homeDir, _ := os.UserHomeDir()
				possiblePaths := []string{
					keystorePath,
					filepath.Join(homeDir, ".marchat", "keystore.dat"),
					filepath.Join(homeDir, ".config", "marchat", "keystore.dat"),
					"./keystore.dat",
				}

				log.Printf("Checking for keystore in possible locations:")
				for _, path := range possiblePaths {
					if stat, err := os.Stat(path); err == nil {
						log.Printf("  Found keystore at: %s (size: %d bytes)", path, stat.Size())
					} else {
						log.Printf("  Not found: %s", path)
					}
				}

				// Check if the file exists and get its size
				if stat, err := os.Stat(keystorePath); err == nil {
					log.Printf("Keystore file exists, size: %d bytes", stat.Size())
				} else {
					log.Printf("Keystore file does not exist or error accessing: %v", err)
				}

				// Check environment variable before keystore creation
				envKey := os.Getenv("MARCHAT_GLOBAL_E2E_KEY")
				if envKey != "" {
					log.Printf("Environment variable MARCHAT_GLOBAL_E2E_KEY is set (length: %d)", len(envKey))
					log.Printf("Environment variable value: %s", envKey)
					// Try to decode to see if it's valid base64
					if decoded, err := base64.StdEncoding.DecodeString(envKey); err == nil {
						log.Printf("Environment variable is valid base64, decoded length: %d bytes", len(decoded))
					} else {
						log.Printf("Environment variable is NOT valid base64: %v", err)
					}
				} else {
					log.Printf("Environment variable MARCHAT_GLOBAL_E2E_KEY is not set")
				}

				// Create keystore AFTER setting environment variable
				keystore = crypto.NewKeyStore(keystorePath)
				if keystore != nil {
					log.Printf("Keystore created successfully")

					// Always use Initialize() first - it handles both new and existing keystores
					// and properly processes the environment variable
					log.Printf("Initializing keystore...")
					err := keystore.Initialize(e2ePassEntry.Text)
					if err != nil {
						log.Printf("Initialize failed: %v", err)
						keystore = nil
					} else {
						log.Printf("Keystore initialized successfully")

						// Check environment variable again after initialization
						envKeyAfter := os.Getenv("MARCHAT_GLOBAL_E2E_KEY")
						if envKeyAfter != "" {
							log.Printf("Environment variable still set after initialization (length: %d)", len(envKeyAfter))
						} else {
							log.Printf("Environment variable is NOT set after initialization!")
						}

						// Verify the global key is available
						testGlobalKey := keystore.GetSessionKey("global")
						if testGlobalKey != nil {
							log.Printf("Global key available via GetSessionKey: KeyID=%s", testGlobalKey.KeyID)
						} else {
							log.Printf("WARNING: Global key is nil via GetSessionKey after initialization!")
							log.Printf("Keystore instance after init: %p", keystore)

							// WORKAROUND: The crypto package is not processing the environment variable correctly
							// This suggests there's a bug in the crypto package's initializeGlobalKey method
							log.Printf("CRYPTO PACKAGE BUG: initializeGlobalKey() is not working properly")
							log.Printf("The environment variable is set but the global key is not being created")
							log.Printf("This is likely a bug in the crypto package that needs to be fixed")

							// For now, we'll keep the keystore but it won't be able to decrypt messages
							// The user will need to either:
							// 1. Use the TUI client which works correctly
							// 2. Wait for the crypto package bug to be fixed
							// 3. Use a different keystore implementation

							// Don't set keystore to nil - let it try to work anyway
							log.Printf("Keeping keystore instance for debugging - it may still work")
						}
					}
				} else {
					log.Printf("Failed to create keystore")
				}
			} else {
				log.Printf("E2E enabled but no passphrase provided")
			}
		}

		// Create GUI using the existing window
		log.Printf("Creating GUI with existing window...")
		log.Printf("Passing keystore instance: %p", keystore)
		adminKey := adminKeyEntry.Text
		gui := NewMarchatGUIWithWindow(fyneApp, window, cfg, keystore, isAdmin, adminKey)

		// The window content will be replaced by setupUI
		// Start the connection in background
		go func() {
			if err := gui.Connect(); err != nil {
				log.Printf("Connection error: %v", err)
				fyne.Do(func() {
					gui.showErrorDialog("Connection Error", err.Error())
				})
			}
		}()
	})
	connectButton.Importance = widget.HighImportance

	cancelButton := widget.NewButton("Cancel", func() {
		fyneApp.Quit()
	})

	buttons := container.NewHBox(
		layout.NewSpacer(),
		cancelButton,
		connectButton,
	)

	content := container.NewVBox(
		widget.NewCard("marchat Configuration", "", form),
		buttons,
	)

	window.SetContent(content)

	// Focus on username entry
	usernameEntry.FocusGained()

	// Show the window (but don't run yet - that will happen in main)
	window.Show()

	// Create a temporary GUI that will be replaced when connect is clicked
	// This is just to satisfy the return type for now
	tempGUI := &MarchatGUI{
		app:      fyneApp,
		window:   window,
		cfg:      nil,
		keystore: nil,
		isAdmin:  false,
		adminKey: "",
	}

	return tempGUI
}
