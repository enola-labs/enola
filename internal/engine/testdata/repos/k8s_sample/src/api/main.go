package main

import "net/http"

func main() {
	http.HandleFunc("/orders", orders)
	_ = http.ListenAndServe(":8080", nil)
}

func orders(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("[]"))
}
