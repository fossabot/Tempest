package tempest

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

type ClientOptions struct {
	Rest                       rest
	ApplicationId              Snowflake                                                 // Your app/bot's user id.
	PublicKey                  string                                                    // Hash like key used to verify incoming payloads from Discord.
	InteractionHandler         func(interaction Interaction)                             // Function to call on all unhandled interactions.
	PreCommandExecutionHandler func(commandInteraction CommandInteraction) *ResponseData // Function to call after doing initial processing but before executing slash command. Allows to attach own, global logic to all slash commands (similar to routing). Return pointer to ResponseData struct if you want to send messageand stop execution or <nil> to continue.
}

type client struct {
	Rest          rest
	User          User
	ApplicationId Snowflake
	PublicKey     ed25519.PublicKey

	commands                   map[string]map[string]Command                             // Search by command name, then subcommand name (if it's main command then provide "-" as subcommand name)
	queuedButtons              map[string]*queuedButton                                  // Map with all currently running button queues.
	interactionHandler         func(interaction Interaction)                             // From options, called on all unhandled interactions.
	preCommandExecutionHandler func(commandInteraction CommandInteraction) *ResponseData // From options, called before each slash command.
	running                    bool                                                      // Whether client's web server is already launched.
}

// Returns time it took to communicate with Discord API (in milliseconds).
func (client client) GetLatency() int64 {
	start := time.Now()
	client.Rest.Request("GET", "/gateway", nil)
	return time.Since(start).Milliseconds()
}

// Adds button & filter to client's button queue. Await for data from channel to aknowledge moment when any of listened buttons gets clicked by matching target. It will emit struct with field Timeout = true on timeout.
func (client client) CreateButtonMenu(CustomIds []string, timeout time.Duration, handler func(button *ButtonInteraction)) {
	if time.Second*3 < timeout {
		timeout = time.Second * 3 // Min 3 seconds
	}

	anchor := queuedButton{
		CustomIds: CustomIds,
		Handler:   handler,
	}

	for _, key := range CustomIds {
		client.queuedButtons[key] = &anchor
	}

	time.AfterFunc(timeout, func() {
		for _, key := range CustomIds {
			delete(client.queuedButtons, key)
		}
		handler(nil)
	})
}

func (client client) SendMessage(channelId Snowflake, content Message) (Message, error) {
	raw, err := client.Rest.Request("POST", "/channels/"+channelId.String()+"/messages", content)
	if err != nil {
		return Message{}, err
	}

	res := Message{}
	err = json.Unmarshal(raw, &res)
	if err != nil {
		return Message{}, errors.New("failed to parse received data from discord")
	}

	return res, nil
}

// Use that for simple text messages that won't be modified.
func (client client) SendLinearMessage(channelId Snowflake, content string) (Message, error) {
	raw, err := client.Rest.Request("POST", "/channels/"+channelId.String()+"/messages", Message{Content: content})
	if err != nil {
		return Message{}, err
	}

	res := Message{}
	err = json.Unmarshal(raw, &res)
	if err != nil {
		return Message{}, errors.New("failed to parse received data from discord")
	}

	return res, nil
}

func (client client) EditMessage(channelId Snowflake, messageId Snowflake, content Message) error {
	_, err := client.Rest.Request("PATCH", "/channels/"+channelId.String()+"/messages"+messageId.String(), content)
	if err != nil {
		return err
	}
	return nil
}

func (client client) DeleteMessage(channelId Snowflake, messageId Snowflake) error {
	_, err := client.Rest.Request("DELETE", "/channels/"+channelId.String()+"/messages"+messageId.String(), nil)
	if err != nil {
		return err
	}
	return nil
}

func (client client) CrosspostMessage(channelId Snowflake, messageId Snowflake) error {
	_, err := client.Rest.Request("POST", "/channels/"+channelId.String()+"/messages"+messageId.String()+"/crosspost", nil)
	if err != nil {
		return err
	}
	return nil
}

func (client client) FetchUser(id Snowflake) (User, error) {
	raw, err := client.Rest.Request("GET", "/users/"+id.String(), nil)
	if err != nil {
		return User{}, err
	}

	res := User{}
	json.Unmarshal(raw, &res)
	if err != nil {
		return User{}, errors.New("failed to parse received data from discord")
	}

	return res, nil
}

func (client client) FetchMember(guildId Snowflake, memberId Snowflake) (Member, error) {
	raw, err := client.Rest.Request("GET", "/guilds/"+guildId.String()+"/members/"+memberId.String(), nil)
	if err != nil {
		return Member{}, err
	}

	res := Member{}
	json.Unmarshal(raw, &res)
	if err != nil {
		return Member{}, errors.New("failed to parse received data from discord")
	}

	return res, nil
}

func (client client) RegisterCommand(command Command) {
	if _, ok := client.commands[command.Name]; !ok {
		if command.Options == nil {
			command.Options = []Option{}
		}

		tree := make(map[string]Command)
		tree["-"] = command
		client.commands[command.Name] = tree
		return
	}

	panic("found already registered \"" + command.Name + "\" slash command")
}

func (client client) RegisterSubCommand(subCommand Command, rootCommandName string) {
	if _, ok := client.commands[rootCommandName]; ok {
		client.commands[rootCommandName][subCommand.Name] = subCommand
		return
	}

	panic("missing \"" + rootCommandName + "\" slash command in registry (register root command first before adding subcommands)")
}

// Sync currently cached slash commands to discord API. By default it'll try to make (bulk) global update (limit 100 updates per day), provide array with guild id snowflakes to update data only for specific guilds.
// You can also add second param -> slice with all command names you want to update (whitelist).
func (client client) SyncCommands(guildIds []Snowflake, commandsToInclude []string) {
	payload := parseCommandsToDiscordObjects(&client, commandsToInclude)

	if len(guildIds) == 0 {
		client.Rest.Request("PUT", "/applications/"+client.ApplicationId.String()+"/commands", payload)
		return
	}

	for _, guildId := range guildIds {
		client.Rest.Request("PUT", "/applications/"+client.ApplicationId.String()+"/guilds/"+guildId.String()+"/commands", payload)
	}
}

func (client client) ListenAndServe(address string) error {
	if client.running {
		panic("client's web server is already launched")
	}

	user, err := client.FetchUser(client.ApplicationId)
	if err != nil {
		panic("failed to fetch bot user's details (check if application id is correct & your internet connection)\n")
	}
	client.User = user

	http.HandleFunc("/", client.handleDiscordWebhookRequests)
	return http.ListenAndServe(address, nil)
}

func CreateClient(options ClientOptions) client {
	discordPublicKey, err := hex.DecodeString(options.PublicKey)
	if err != nil {
		panic("failed to decode \"%s\" discord's public key (check if it's correct key)")
	}

	client := client{
		Rest:               options.Rest,
		ApplicationId:      options.ApplicationId,
		PublicKey:          ed25519.PublicKey(discordPublicKey),
		commands:           make(map[string]map[string]Command, 50), // Allocate space for 50 global slash commands
		interactionHandler: options.InteractionHandler,
		running:            false,
	}

	return client
}

func (client client) handleDiscordWebhookRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed.", http.StatusMethodNotAllowed)
		return
	}

	verified := verifyRequest(r, ed25519.PublicKey(client.PublicKey))
	if !verified {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var interaction Interaction
	err := json.NewDecoder(r.Body).Decode(&interaction)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		panic(err)

	}
	defer r.Body.Close()

	interaction.Client = &client // Bind access to client instance which is needed for methods.
	switch interaction.Type {
	case PING_TYPE:
		w.Write([]byte(`{"type":1}`))
		return
	case APPLICATION_COMMAND_TYPE:
		command, interaction, exists := client.getCommand(interaction)
		if !exists {
			terminateCommandInteraction(w)
			return
		}

		if interaction.GuildID == 0 && !command.AvailableInDM {
			w.WriteHeader(http.StatusNoContent)
			return // Stop execution since this command doesn't want to be used inside DM.
		}

		ctx := CommandInteraction(interaction)
		if client.preCommandExecutionHandler != nil {
			content := client.preCommandExecutionHandler(ctx)
			if content != nil {
				body, err := json.Marshal(Response{
					Type: CHANNEL_MESSAGE_WITH_SOURCE_RESPONSE,
					Data: content,
				})

				if err != nil {
					panic("failed to parse payload received from client's \"pre command execution\" handler (make sure it's in JSON format)")
				}

				w.Header().Add("Content-Type", "application/json")
				w.Write(body)
				return
			}
		}

		w.WriteHeader(http.StatusNoContent)
		command.SlashCommandHandler(ctx)
		return
	case MESSAGE_COMPONENT_TYPE:
		switch interaction.Data.ComponentType {
		case COMPONENT_BUTTON:
			queue, exists := client.queuedButtons[interaction.Data.CustomId]

			if exists {
				ctx := ButtonInteraction(interaction)
				queue.Handler(&ctx)

				for _, key := range queue.CustomIds {
					delete(client.queuedButtons, key)
				}
			}

			if client.interactionHandler != nil {
				client.interactionHandler(interaction)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			if client.interactionHandler != nil {
				client.interactionHandler(interaction)
			}
		}

		return
	case APPLICATION_COMMAND_AUTO_COMPLETE_TYPE:
		command, interaction, exists := client.getCommand(interaction)
		if !exists || command.AutoCompleteHandler == nil || len(command.Options) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		choices := command.AutoCompleteHandler(AutoCompleteInteraction(interaction))
		body, err := json.Marshal(ResponseChoice{
			Type: AUTOCOMPLETE_RESPONSE,
			Data: ResponseChoiceData{
				Choices: choices,
			},
		})

		if err != nil {
			panic("failed to parse payload received from client's \"auto complete\" handler (make sure it's in JSON format)")
		}

		w.Header().Add("Content-Type", "application/json")
		w.Write(body)
		return
	default:
		if client.interactionHandler != nil {
			client.interactionHandler(interaction)
		}
	}
}

// Returns command, subcommand, a command context (updated interaction) and bool to check whether it suceeded and is safe to use.
func (client client) getCommand(interaction Interaction) (Command, Interaction, bool) {
	if len(interaction.Data.Options) != 0 && interaction.Data.Options[0].Type == OPTION_SUB_COMMAND {
		rootName := interaction.Data.Name
		interaction.Data.Name, interaction.Data.Options = interaction.Data.Options[0].Name, interaction.Data.Options[0].Options
		command, exists := client.commands[rootName][interaction.Data.Name]
		if !exists {
			return Command{}, interaction, false
		}
		return command, interaction, true
	}

	command, exists := client.commands[interaction.Data.Name]["-"]
	if !exists {
		return Command{}, interaction, false
	}

	return command, interaction, true
}
