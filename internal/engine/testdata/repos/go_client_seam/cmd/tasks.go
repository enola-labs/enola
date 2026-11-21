package cmd

import "example.com/seamcli/api"

func ListTasks() ([]byte, error) {
	return api.Get("/me/tasks")
}
