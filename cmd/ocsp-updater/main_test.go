package main

import (
	"crypto/x509"
	"sync"
	"testing"
	"time"

	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/jmhodges/clock"
	"github.com/letsencrypt/boulder/Godeps/_workspace/src/gopkg.in/gorp.v1"
	"github.com/letsencrypt/boulder/core"

	"github.com/letsencrypt/boulder/sa"
	"github.com/letsencrypt/boulder/sa/satest"
	"github.com/letsencrypt/boulder/test"
)

type mockCA struct{}

func (ca *mockCA) IssueCertificate(csr x509.CertificateRequest, regID int64) (core.Certificate, error) {
	return core.Certificate{}, nil
}

func (ca *mockCA) GenerateOCSP(xferObj core.OCSPSigningRequest) (ocsp []byte, err error) {
	ocsp = []byte{1, 2, 3}
	return
}

func (ca *mockCA) RevokeCertificate(serial string, reasonCode core.RevocationCode) (err error) {
	return
}

type mockPub struct {
	sa core.StorageAuthority
}

func (p *mockPub) SubmitToCT(_ []byte) error {
	return p.sa.AddSCTReceipt(core.SignedCertificateTimestamp{
		SCTVersion:        0,
		LogID:             "id",
		Timestamp:         0,
		Extensions:        []byte{},
		Signature:         []byte{0},
		CertificateSerial: "00",
	})
}

const dbConnStr = "mysql+tcp://boulder@localhost:3306/boulder_sa_test"

func setup(t *testing.T) (OCSPUpdater, core.StorageAuthority, *gorp.DbMap, clock.FakeClock, func()) {
	dbMap, err := sa.NewDbMap(dbConnStr)
	test.AssertNotError(t, err, "Failed to create dbMap")

	fc := clock.NewFake()
	fc.Add(1 * time.Hour)

	sa, err := sa.NewSQLStorageAuthority(dbMap, fc)
	test.AssertNotError(t, err, "Failed to create SA")

	cleanUp := test.ResetTestDatabase(t, dbMap.Db)

	updater := OCSPUpdater{
		dbMap: dbMap,
		clk:   fc,
		cac:   &mockCA{},
		pub:   &mockPub{sa},
		rpcMu: new(sync.RWMutex),
	}

	return updater, sa, dbMap, fc, cleanUp
}

func TestGenerateAndStoreOCSPResponse(t *testing.T) {
	updater, sa, dbMap, _, cleanUp := setup(t)
	defer cleanUp()

	reg := satest.CreateWorkingRegistration(t, sa)
	parsedCert, err := core.LoadCert("test-cert.pem")
	test.AssertNotError(t, err, "Couldn't read test certificate")
	_, err = sa.AddCertificate(parsedCert.Raw, reg.ID)
	test.AssertNotError(t, err, "Couldn't add www.eff.org.der")

	cert, err := sa.GetCertificate(core.SerialToString(parsedCert.SerialNumber))
	test.AssertNotError(t, err, "Couldn't get the core.Certificate from the database")

	meta, err := updater.generateResponse(cert)
	test.AssertNotError(t, err, "Couldn't generate OCSP response")
	tx, err := dbMap.Begin()
	test.AssertNotError(t, err, "Couldn't open a transaction")
	err = updater.storeResponse(tx, meta)
	test.AssertNotError(t, err, "Couldn't store OCSP response")
	err = tx.Commit()
	test.AssertNotError(t, err, "Couldn't close transaction")

	var ocspResponse core.OCSPResponse
	err = dbMap.SelectOne(
		&ocspResponse,
		"SELECT * from ocspResponses WHERE serial = :serial ORDER BY id DESC LIMIT 1;",
		map[string]interface{}{"serial": cert.Serial},
	)
	test.AssertNotError(t, err, "Couldn't get OCSP response from database")
}

func TestGenerateOCSPResponses(t *testing.T) {
	updater, sa, _, fc, cleanUp := setup(t)
	defer cleanUp()

	reg := satest.CreateWorkingRegistration(t, sa)
	parsedCert, err := core.LoadCert("test-cert.pem")
	test.AssertNotError(t, err, "Couldn't read test certificate")
	_, err = sa.AddCertificate(parsedCert.Raw, reg.ID)
	test.AssertNotError(t, err, "Couldn't add test-cert.pem")
	parsedCert, err = core.LoadCert("test-cert-b.pem")
	test.AssertNotError(t, err, "Couldn't read test certificate")
	_, err = sa.AddCertificate(parsedCert.Raw, reg.ID)
	test.AssertNotError(t, err, "Couldn't add test-cert-b.pem")

	earliest := fc.Now().Add(-time.Hour)
	certs, err := updater.findStaleOCSPResponses(earliest, 10)
	test.AssertNotError(t, err, "Couldn't find stale responses")
	test.AssertEquals(t, len(certs), 2)

	wg := new(sync.WaitGroup)
	updater.generateOCSPResponses(wg, certs)
	wg.Wait()

	certs, err = updater.findStaleOCSPResponses(earliest, 10)
	test.AssertError(t, err, "Should have been a nothing found error")
	test.AssertEquals(t, err, errNoStaleResponses)
}

func TestFindStaleOCSPResponses(t *testing.T) {
	updater, sa, dbMap, fc, cleanUp := setup(t)
	defer cleanUp()

	reg := satest.CreateWorkingRegistration(t, sa)
	parsedCert, err := core.LoadCert("test-cert.pem")
	test.AssertNotError(t, err, "Couldn't read test certificate")
	_, err = sa.AddCertificate(parsedCert.Raw, reg.ID)
	test.AssertNotError(t, err, "Couldn't add www.eff.org.der")

	earliest := fc.Now().Add(-time.Hour)
	certs, err := updater.findStaleOCSPResponses(earliest, 10)
	test.AssertNotError(t, err, "Couldn't find certificate")
	test.AssertEquals(t, len(certs), 1)

	cert, err := sa.GetCertificate(core.SerialToString(parsedCert.SerialNumber))
	test.AssertNotError(t, err, "Couldn't get the core.Certificate from the database")

	meta, err := updater.generateResponse(cert)
	test.AssertNotError(t, err, "Couldn't generate OCSP response")
	tx, err := dbMap.Begin()
	test.AssertNotError(t, err, "Couldn't open a transaction")
	err = updater.storeResponse(tx, meta)
	test.AssertNotError(t, err, "Couldn't store OCSP response")
	err = tx.Commit()
	test.AssertNotError(t, err, "Couldn't close transaction")

	certs, err = updater.findStaleOCSPResponses(earliest, 10)
	test.AssertError(t, err, "Should have been a nothing found error")
	test.AssertEquals(t, err, errNoStaleResponses)
}

func TestGetNumberOfReceipts(t *testing.T) {
	updater, sa, _, _, cleanUp := setup(t)
	defer cleanUp()

	reg := satest.CreateWorkingRegistration(t, sa)
	cert, err := core.LoadCert("test-cert.pem")
	test.AssertNotError(t, err, "Couldn't read test certificate")
	_, err = sa.AddCertificate(cert.Raw, reg.ID)
	test.AssertNotError(t, err, "Couldn't add www.eff.org.der")

	updater.numLogs = 1

	count, err := updater.getNumberOfReceipts(core.SerialToString(cert.SerialNumber))
	test.AssertNotError(t, err, "Couldn't get number of receipts")
	test.AssertEquals(t, count, 0)

	err = sa.AddSCTReceipt(core.SignedCertificateTimestamp{
		SCTVersion:        0,
		LogID:             "id",
		Timestamp:         0,
		Extensions:        []byte{},
		Signature:         []byte{0},
		CertificateSerial: core.SerialToString(cert.SerialNumber),
	})
	test.AssertNotError(t, err, "Couldn't add SCT receipt to database")

	count, err = updater.getNumberOfReceipts(core.SerialToString(cert.SerialNumber))
	test.AssertNotError(t, err, "Couldn't get number of receipts")
	test.AssertEquals(t, count, 1)
}

func TestGetCertificatesIssuedSince(t *testing.T) {
	updater, sa, _, fc, cleanUp := setup(t)
	defer cleanUp()

	reg := satest.CreateWorkingRegistration(t, sa)
	cert, err := core.LoadCert("test-cert.pem")
	test.AssertNotError(t, err, "Couldn't read test certificate")
	_, err = sa.AddCertificate(cert.Raw, reg.ID)
	test.AssertNotError(t, err, "Couldn't add www.eff.org.der")

	since := fc.Now().Add(-time.Hour)
	certs, err := updater.getCertificatesIssuedSince(since, 10)
	test.AssertNotError(t, err, "Couldn't get certificate")
	test.AssertEquals(t, len(certs), 1)

	fc.Add(time.Hour)
	since = fc.Now().Add(-time.Hour)
	certs, err = updater.getCertificatesIssuedSince(since, 10)
	test.AssertError(t, err, "Should have been a nothing found error")
	test.AssertEquals(t, err, errNoNewCertificates)
}

func TestNewCertificateTick(t *testing.T) {
	updater, sa, _, fc, cleanUp := setup(t)
	defer cleanUp()

	reg := satest.CreateWorkingRegistration(t, sa)
	parsedCert, err := core.LoadCert("test-cert.pem")
	test.AssertNotError(t, err, "Couldn't read test certificate")
	_, err = sa.AddCertificate(parsedCert.Raw, reg.ID)
	test.AssertNotError(t, err, "Couldn't add www.eff.org.der")

	prev := fc.Now().Add(-time.Hour)
	updater.newCertificateTick(10, time.Time{}, prev)

	_, err = updater.findStaleOCSPResponses(prev, 10)
	test.AssertError(t, err, "Should have been a nothing found error")
	test.AssertEquals(t, err, errNoStaleResponses)

	count, err := updater.getNumberOfReceipts("00")
	test.AssertNotError(t, err, "Couldn't get number of receipts")
	test.AssertEquals(t, count, 1)
}

func TestMissingReceiptsTick(t *testing.T) {
	updater, sa, _, fc, cleanUp := setup(t)
	defer cleanUp()

	reg := satest.CreateWorkingRegistration(t, sa)
	parsedCert, err := core.LoadCert("test-cert.pem")
	test.AssertNotError(t, err, "Couldn't read test certificate")
	_, err = sa.AddCertificate(parsedCert.Raw, reg.ID)
	test.AssertNotError(t, err, "Couldn't add www.eff.org.der")

	updater.numLogs = 1
	updater.oldestIssuedSCT = 1 * time.Hour
	updater.missingReceiptsTick(10, fc.Now(), time.Time{})

	count, err := updater.getNumberOfReceipts("00")
	test.AssertNotError(t, err, "Couldn't get number of receipts")
	test.AssertEquals(t, count, 1)
}

func TestOldOCSPResponsesTick(t *testing.T) {
	updater, sa, _, fc, cleanUp := setup(t)
	defer cleanUp()

	reg := satest.CreateWorkingRegistration(t, sa)
	parsedCert, err := core.LoadCert("test-cert.pem")
	test.AssertNotError(t, err, "Couldn't read test certificate")
	_, err = sa.AddCertificate(parsedCert.Raw, reg.ID)
	test.AssertNotError(t, err, "Couldn't add www.eff.org.der")

	updater.numLogs = 1
	updater.ocspMinTimeToExpiry = 1 * time.Hour
	updater.oldOCSPResponsesTick(10, fc.Now(), time.Time{})

	_, err = updater.findStaleOCSPResponses(fc.Now().Add(-updater.ocspMinTimeToExpiry), 10)
	test.AssertError(t, err, "Should have been a nothing found error")
	test.AssertEquals(t, err, errNoStaleResponses)
}