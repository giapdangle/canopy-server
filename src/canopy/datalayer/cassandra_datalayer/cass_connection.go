/*
 * Copyright 2014 Gregory Prisament
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package cassandra_datalayer

import(
    "canopy/canolog"
    "canopy/datalayer"
    "canopy/sddl"
    "canopy/util/random"
    "fmt"
    "github.com/gocql/gocql"
    "code.google.com/p/go.crypto/bcrypt"
    "regexp"
)

type CassConnection struct {
    dl *CassDatalayer
    session *gocql.Session
}

// Use with care.  Erases all sensor data.
func (conn *CassConnection) ClearSensorData() {
    tables := []string{
        "propval_int",
        "propval_float",
        "propval_double",
        "propval_timestamp",
        "propval_boolean",
        "propval_void",
        "propval_string",
    }
    for _, table := range tables {
        err := conn.session.Query(`TRUNCATE ` + table).Exec();
        if (err != nil) {
            canolog.Error("Error truncating ", table, ":", err)
        }
    }
}

func (conn *CassConnection) Close() {
    conn.session.Close()
}

func validateUsername(username string) error {
    if username == "leela" {
        return fmt.Errorf("Username reserved")
    }
    if len(username) < 5 {
        return fmt.Errorf("Username too short")
    }
    if len(username) > 24 {
        return fmt.Errorf("Username too long")
    }
    matched, err := regexp.MatchString("[a-zA-Z][a-zA-Z0-9_]+", username)
    if !matched || err != nil {
        return fmt.Errorf("Invalid characters in username")
    }

    return nil
}

func validatePassword(password string) error {
    if len(password) < 6 {
        return fmt.Errorf("Password too short")
    }
    if len(password) > 120 {
        return fmt.Errorf("Password too long")
    }
    return nil
}

func validateEmail(email string) error {
    // TODO
    return nil
}

func (conn *CassConnection) CreateAccount(username, email, password string) (datalayer.Account, error) {
    password_hash, _ := bcrypt.GenerateFromPassword([]byte(password + salt), hashCost)

    err := validateUsername(username)
    if err != nil {
        return nil, err
    }

    err = validateEmail(email)
    if err != nil {
        return nil, err
    }

    err = validatePassword(password)
    if err != nil {
        return nil, err
    }

    activation_code, err := random.Base64String(24)
    if err != nil {
        return nil, err
    }

    // TODO: transactionize
    if err := conn.session.Query(`
            INSERT INTO accounts (username, email, password_hash, activated, activation_code)
            VALUES (?, ?, ?, ?, ?)
    `, username, email, password_hash, false, activation_code).Exec(); err != nil {
        canolog.Error("Error creating account:", err)
        return nil, err
    }

    if err := conn.session.Query(`
            INSERT INTO account_emails (email, username)
            VALUES (?, ?)
    `, email, username).Exec(); err != nil {
        canolog.Error("Error setting account email:", err)
        return nil, err
    }

    return &CassAccount{conn, username, email, password_hash, false, activation_code}, nil
}

func (conn *CassConnection) CreateDevice(name string, uuid *gocql.UUID, secretKey string, publicAccessLevel datalayer.AccessLevel) (datalayer.Device, error) {
    // TODO: validate parameters 
    var id gocql.UUID
    var err error

    if uuid == nil {
        id, err = gocql.RandomUUID()
        if err != nil {
            return nil, err
        }
    } else {
        id = *uuid
    }
    
    if secretKey == "" {
        secretKey, err = random.Base64String(24)
        if err != nil {
            return nil, err
        }
    }
    
    err = conn.session.Query(`
            INSERT INTO devices (device_id, secret_key, friendly_name, public_access_level)
            VALUES (?, ?, ?, ?)
    `, id, secretKey, name, publicAccessLevel).Exec()
    if err != nil {
        canolog.Error("Error creating device:", err)
        return nil, err
    }
    return &CassDevice{
        conn: conn,
        deviceId: id,
        secretKey: secretKey,
        name: name,
        doc: sddl.Sys.NewEmptyDocument(),
        docString: "",
        publicAccessLevel: publicAccessLevel,
    }, nil
}

func (conn *CassConnection) LookupOrCreateDevice(deviceId gocql.UUID, publicAccessLevel datalayer.AccessLevel) (datalayer.Device, error) {
    // TODO: improve this implementation.
    // Fix race conditions?
    // Fix error paths?
    
    device, err := conn.LookupDevice(deviceId)
    if device != nil {
        canolog.Info("LookupOrCreateDevice - device ", deviceId, " found")
        return device, nil
    }

    device, err = conn.CreateDevice("AnonDevice", &deviceId, "", publicAccessLevel)
    if err != nil {
        canolog.Info("LookupOrCreateDevice - device ", deviceId, "error")
    }
    canolog.Info("LookupOrCreateDevice - device ", deviceId, " created")
    return device, err
}

func (conn *CassConnection) DeleteAccount(username string) {
    account, _ := conn.LookupAccount(username)
    email := account.Email()

    if err := conn.session.Query(`
            DELETE FROM accounts
            WHERE username = ?
    `, username).Exec(); err != nil {
        canolog.Error("Error deleting account", err)
    }

    if err := conn.session.Query(`
            DELETE FROM account_emails
            WHERE email = ?
    `, email).Exec(); err != nil {
        canolog.Error("Error deleting account email", err)
    }
}

func (conn *CassConnection) LookupAccount(usernameOrEmail string) (datalayer.Account, error) {
    var account CassAccount

    if err := conn.session.Query(`
            SELECT username, email, password_hash, activated, activation_code FROM accounts 
            WHERE username = ?
            LIMIT 1
    `, usernameOrEmail).Consistency(gocql.One).Scan(
         &account.username, &account.email, &account.password_hash, &account.activated, &account.activation_code); err != nil {
            canolog.Error("Error looking up account", err)
            return nil, err
    }
    /* TODO: try email if username not found */
    account.conn = conn
    return &account, nil
}

func (conn *CassConnection)LookupAccountVerifyPassword(usernameOrEmail string, password string) (datalayer.Account, error) {
    account, err := conn.LookupAccount(usernameOrEmail)
    if err != nil {
        return nil, err
    }

    verified := account.VerifyPassword(password)
    if (!verified) {
        canolog.Info("Incorrect password for ", usernameOrEmail)
        return nil, datalayer.InvalidPasswordError
    }

    return account, nil
}

func (conn *CassConnection) LookupDevice(deviceId gocql.UUID) (datalayer.Device, error) {
    var device CassDevice

    device.deviceId = deviceId
    device.conn = conn

    err := conn.session.Query(`
        SELECT friendly_name, secret_key, sddl
        FROM devices
        WHERE device_id = ?
        LIMIT 1`, deviceId).Consistency(gocql.One).Scan(
            &device.name,
            &device.secretKey,
            &device.docString)
    if err != nil {
        return nil, err
    }

    if device.docString != "" {
        device.doc, err = sddl.Sys.ParseDocumentString(device.docString)
        if err != nil {
            canolog.Error("Error parsing class string for device: ", device.docString, err)
            return nil, err
        }
    } else {
        device.doc = sddl.Sys.NewEmptyDocument()
    }

    return &device, nil
}

func (conn *CassConnection) LookupDeviceByStringID(id string) (datalayer.Device, error) {
    deviceId, err := gocql.ParseUUID(id)
    if err != nil {
        canolog.Error(err)
        return nil, err
    }
    return conn.LookupDevice(deviceId)
}

