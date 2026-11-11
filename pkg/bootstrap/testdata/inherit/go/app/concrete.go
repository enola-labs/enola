package app

type Concrete struct {
	Base
	repo *Repo
}

func NewConcrete() *Concrete { return &Concrete{Base: NewBase(), repo: &Repo{}} }
func (c *Concrete) Step()    { c.repo.Save() }
func (c *Concrete) Handle()  { c.Common(); c.helper.Work() }
