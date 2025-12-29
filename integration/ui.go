package main

import (
	"github.com/gdamore/tcell/v2"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/nustiueudinastea/doltswarm"
	"github.com/rivo/tview"
)

func uiUpdate(app *tview.Application, peerListView *tview.List, commitTreeRoot *tview.TreeNode, textView *tview.TextView, peerListChan chan peer.IDSlice, commitListChan chan []doltswarm.Commit, eventChan chan []byte) func() error {
	stopSignal := make(chan struct{})
	go func() {
		log.Info("Starting UI updater")
		for {
			select {
			case peerList := <-peerListChan:
				peerListView.Clear()
				for _, peer := range peerList {
					peerListView.AddItem(peer.ShortString(), "", 0, nil)
				}
				app.Draw()
			case event := <-eventChan:
				_, err := textView.Write(event)
				if err != nil {
					panic(err)
				}
				app.Draw()
			case commitList := <-commitListChan:
				commitTreeRoot.ClearChildren()
				for _, commit := range commitList {
					// Show short committer ID (last 16 chars) if available
					committer := commit.Committer
					if len(committer) > 16 {
						committer = "..." + committer[len(committer)-16:]
					}
					node := tview.NewTreeNode(commit.Hash + " [" + committer + "] " + commit.Message)
					commitTreeRoot.AddChild(node)
				}
				app.Draw()
			case <-stopSignal:
				log.Info("Stopping ui updater")
				return
			}
		}
	}()
	stopper := func() error {
		stopSignal <- struct{}{}
		return nil
	}
	return stopper
}

func createUI(peerListChan chan peer.IDSlice, commitListChan chan []doltswarm.Commit, eventChan chan []byte) *tview.Application {
	var app = tview.NewApplication()
	var flex = tview.NewFlex()

	flexRow := tview.NewFlex().SetDirection(tview.FlexRow)

	commitTreeRoot := tview.NewTreeNode(".").SetColor(tcell.ColorRed)
	commitTree := tview.NewTreeView().SetRoot(commitTreeRoot)
	commitTree.SetBorder(true).SetTitle("Commits")

	logView := tview.NewTextView()
	logView.SetBorder(true).SetTitle("Events")

	peerList := tview.NewList()
	peerList.SetBorder(true).SetTitle("Peers")

	flexRow.AddItem(peerList, 0, 1, false).
		AddItem(commitTree, 0, 5, false)

	flex.AddItem(flexRow, 0, 1, false).
		AddItem(logView, 0, 1, false)

	uiUpdateStopper := uiUpdate(app, peerList, commitTreeRoot, logView, peerListChan, commitListChan, eventChan)
	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyCtrlC {
			uiUpdateStopper()
			app.Stop()
		}
		return event
	})

	app.SetRoot(flex, true)
	return app

}
