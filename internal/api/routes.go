package api

import (
	"github.com/gofiber/fiber/v2"
)

// Mount attaches the versioned API. When deps == nil (scaffold-only mode)
// only a trivial ping is mounted.
func Mount(app *fiber.App, d *Deps) {
	v1 := app.Group("/v1")
	v1.Get("/ping", func(c *fiber.Ctx) error { return c.JSON(fiber.Map{"pong": true}) })
	if d == nil {
		return
	}

	// All write routes are authenticated.
	authed := v1.Group("", apiKeyMiddleware(d.APIKey))

	authed.Post("/notifications", createNotification(d))
	authed.Get("/notifications/:id", getNotification(d))

	authed.Get("/templates", listTemplates(d))
	authed.Post("/templates", upsertTemplate(d))
	authed.Get("/templates/:id", getTemplate(d))
	authed.Delete("/templates/:id", deleteTemplate(d))

	authed.Post("/users", upsertUser(d))
	authed.Get("/users/:id/preferences", getPreferences(d))
	authed.Put("/users/:id/preferences", putPreference(d))
	authed.Get("/users/:id/in-app-notifications", listInApp(d))
	authed.Post("/users/:id/in-app-notifications/:nid/read", markInAppRead(d))
}
