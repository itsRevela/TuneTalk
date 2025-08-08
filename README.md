# 🎵 TuneTalk

TuneTalk is a simple, lightweight Discord bot written in Go. It's designed to join a voice channel and play local audio files from a pre-configured directory, controlled by easy-to-use slash commands.

---

## Features

-   **Slash Commands**: Modern and intuitive user interaction.
-   **Interactive Menus**: Paginated menus to easily browse a large library of sounds.
-   **Local Audio**: Plays audio files directly from the server where the bot is hosted.
-   **Secure**: Uses a `.env` file to keep your Discord bot token private and out of the codebase.

---

## ⚙️ Prerequisites

Before you can get TuneTalk running, you need to have a few things installed and set up on the machine where you'll host the bot.

-   **Go**: The Go programming language (version 1.18 or higher is recommended).
-   **FFmpeg**: A command-line tool for handling audio and video. It must be installed and accessible in your system's PATH.
-   **Discord Bot Token**: You need to create a Discord Application and a Bot to get a token. You can do this at the [Discord Developer Portal](https://discord.com/developers/applications).
-   **Discord Bot Permissions**: Presence/Members/Message Intents turned on with the scopes "applications.commands" & "bot" (permissions = connect, send messages, speak, use voice activity, view channels).

---

## 🚀 Getting Started

There are two primary ways to run this bot: locally on your own machine for development or on a Pterodactyl panel for hosting.

### TuneTalk Setup:

This is the best way to test and develop the bot.

1.  **Clone the Repository**
    Open your terminal and clone the project from GitHub.
    ```bash
    git clone [https://github.com/itsRevela/TuneTalk.git](https://github.com/itsRevela/TuneTalk.git)
    cd TuneTalk
    ```

2.  **Create Your Environment File**
    The bot needs your Discord token to log in. Create a file named `.env` in the root of the project directory.
    ```
    # .env
    DISCORD_TOKEN=YourBotTokenHere
    ```
    *Replace `YourBotTokenHere` with your actual bot token.*

3.  **Install Dependencies**
    This command will download the necessary Go libraries defined in `go.mod`.
    ```bash
    go mod tidy
    ```

4.  **Run the Bot**
    You're all set! Start the bot with this command.
    ```bash
    go run main.go
    ```
    You should see a log message in your terminal saying "Bot is running."

---

## 🤖 Bot Usage

Once the bot is running and invited to your Discord server, you can use the following slash commands:

-   **/sounds**: This command opens an interactive, ephemeral message with a dropdown menu. You can browse through your audio files and select one to play. The bot will then ask you which voice channel to join.
-   **/stop**: This command will immediately stop any audio playback, and the bot will disconnect from the voice channel.
