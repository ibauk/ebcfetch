# ebcfetch
A fetcher for Electronic Bonus Claims

I monitor an email mailbox for emailed bonus claims from rally entrants, select those with correctly formatted Subject lines and a single
attached image, and post the contents into the specified ScoreMaster database.

On startup I look for an email account specification in the configuration file *ebcfetch.yml*. If one is not specified or has a blank
password, I issue an error message and die.