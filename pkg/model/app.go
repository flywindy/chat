package model

// App is a read-only view of the apps collection (provisioning is upstream).
type App struct {
	ID          string        `json:"id"                    bson:"_id"`
	Name        string        `json:"name"                  bson:"name"`
	Description string        `json:"description,omitempty" bson:"description,omitempty"`
	Assistant   *AppAssistant `json:"assistant,omitempty"   bson:"assistant,omitempty"`
	Sponsors    []AppSponsor  `json:"sponsors,omitempty"    bson:"sponsors,omitempty"`
}

// AppAssistant: Name is the bot user account (".bot" suffix); botDM requires Enabled==true.
type AppAssistant struct {
	Enabled     bool   `json:"enabled"               bson:"enabled"`
	Name        string `json:"name"                  bson:"name"`
	SettingsURL string `json:"settingsUrl,omitempty" bson:"settingsUrl,omitempty"`
}

type AppSponsor struct {
	Name  string `json:"name"  bson:"name"`
	Phone string `json:"phone" bson:"phone"`
}
