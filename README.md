# ebcfetch
A fetcher for Electronic Bonus Claims

I monitor an email mailbox for emailed bonus claims from rally entrants, select those with correctly formatted Subject lines and a single
attached image, and post the contents into the specified ScoreMaster database.

On startup I open a ScoreMaster database (dbversion >= 8) and retrieve my configuration from it unless the name of a yaml configuration file was supplied on the commandline. If no email account is specified or its password is blank, I issue an error message and die.