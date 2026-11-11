package app

import "fmt"

type Repo struct{}

func (r *Repo) Save() { fmt.Println("save") }

type Helper struct{}

func (h *Helper) Work() { fmt.Println("work") }

type Base struct{ helper *Helper }

func NewBase() Base     { return Base{helper: &Helper{}} }
func (b *Base) Common() { b.helper.Work() }
