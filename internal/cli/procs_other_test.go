//go:build !unix

package main

func killDescendants(int) error { return nil }
