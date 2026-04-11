# militia-discord-bot

A Go-based Discord utility that handles scheduled, recurring pings to webhooks (ping relays), specific channel IDs, or a static internal channel.

## 🚀 Getting Started

### Prerequisites
* **Go** (1.18 or higher)
* **A Discord Bot Token**

### Installation
To clone the repository, use the following command:

```bash
git clone https://github.com/mf-km/militia-discord-bot
cd militia-discord-bot
```

### Configuration
You must configure the bot before building it:

    Locate config.example.json.
    Save a copy of it as config.json.
    Fill in your token, channel_ids, and webhook_urls as required.

    Important: config.json is ignored by git to protect your sensitive tokens. Do not remove it from .gitignore.

### Building and Running
```bash

#Fetch dependencies
go mod tidy

# Build the executable
go build -o ping-bot

# Run the bot
./ping-bot

# or, add it to your supervisor config to run full time
# google that for your system
```
### 🛠 Features & Commands

    Configure the bot to listen in a specific discord channel, and control access via discord permissions.
    .ping <message> | sends to your internal alliance pings channel by channel ID.
    .pingmil <message> | sends message to a list of webhook URLs for ping relay functionality
    .pingic24 <message> | works similar to .pingmil but pings a list of channel IDs for servers the bot has been invited to.
    .pinglater <alliance|militia|ic24> YYYYMMDD HH:MM <message> | schedule a one-time ping (UTC, confirms before scheduling)
    .pingschedule add alliance|militia|ic24 <min> <hour> <dom> <month> <dow> <message> | add a recurring scheduled ping
    .pingschedule remove <id> | remove a recurring scheduled ping by ID
    .pingschedule list | show all recurring scheduled pings and next run times
    .help | this message

Help Menu and example of using .pinglater and canceling to re-do the time  
<img width="625" height="742" alt="militia-discord-bot-example" src="https://github.com/user-attachments/assets/d697c5f9-1915-46e4-86bd-e52ee7d7a138" />
