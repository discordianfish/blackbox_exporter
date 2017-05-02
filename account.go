package main

type accountNoop struct{}

func (a *accountNoop) Book() error {
	return nil
}
