package db

import (
	"context"
	sqldriver "database/sql/driver"
)

//
// Connector
//

type Connector struct {
	driver sqldriver.Driver
}

func (c Connector) Connect(_ context.Context) (sqldriver.Conn, error) {
	return c.driver.Open("")
}

func (c Connector) Driver() sqldriver.Driver {
	return c.driver
}

//
// Driver
//

type doltDriver struct {
	conn sqldriver.Conn
}

func (d *doltDriver) Open(dataSource string) (sqldriver.Conn, error) {
	return d.conn, nil
}
