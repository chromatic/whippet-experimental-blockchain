// Copyright (c) 2009-2014 The Bitcoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#include "uritests.h"

#include "guiutil.h"
#include "walletmodel.h"

#include <QUrl>

void URITests::uriTests()
{
    SendCoinsRecipient rv;
    QUrl uri;
    uri.setUrl(QString("whippet:WWbFn7iZqbYR56ceXiKuXs2UyYU2pRqewS?req-dontexist="));
    QVERIFY(!GUIUtil::parseBitcoinURI(uri, &rv));

    uri.setUrl(QString("whippet:WVGSSRbxecQ4PhMTCmNsuC1t2BEa1vzH3a?dontexist="));
    qDebug() << "scheme:" << uri.scheme() << "host:" << uri.host() << "path:" << uri.path();
    QVERIFY(GUIUtil::parseBitcoinURI(uri, &rv));
    QCOMPARE(rv.address, QString("WVGSSRbxecQ4PhMTCmNsuC1t2BEa1vzH3a"));
    QVERIFY(rv.label == QString());
    QVERIFY(rv.amount == 0);

    uri.setUrl(QString("whippet:WWbFn7iZqbYR56ceXiKuXs2UyYU2pRqewS?label=Wikipedia Example Address"));
    QVERIFY(GUIUtil::parseBitcoinURI(uri, &rv));
    QCOMPARE(rv.address, QString("WWbFn7iZqbYR56ceXiKuXs2UyYU2pRqewS"));
    QVERIFY(rv.label == QString("Wikipedia Example Address"));
    QVERIFY(rv.amount == 0);

    uri.setUrl(QString("whippet:WWbFn7iZqbYR56ceXiKuXs2UyYU2pRqewS?amount=0.001"));
    QVERIFY(GUIUtil::parseBitcoinURI(uri, &rv));
    QCOMPARE(rv.address, QString("WWbFn7iZqbYR56ceXiKuXs2UyYU2pRqewS"));
    QVERIFY(rv.label == QString());
    QVERIFY(rv.amount == 100000);

    uri.setUrl(QString("whippet:WWbFn7iZqbYR56ceXiKuXs2UyYU2pRqewS?amount=1.001"));
    QVERIFY(GUIUtil::parseBitcoinURI(uri, &rv));
    QCOMPARE(rv.address, QString("WWbFn7iZqbYR56ceXiKuXs2UyYU2pRqewS"));
    QVERIFY(rv.label == QString());
    QVERIFY(rv.amount == 100100000);

    uri.setUrl(QString("whippet:WWbFn7iZqbYR56ceXiKuXs2UyYU2pRqewS?amount=100&label=Wikipedia Example"));
    QVERIFY(GUIUtil::parseBitcoinURI(uri, &rv));
    QCOMPARE(rv.address, QString("WWbFn7iZqbYR56ceXiKuXs2UyYU2pRqewS"));
    QVERIFY(rv.amount == 10000000000LL);
    QVERIFY(rv.label == QString("Wikipedia Example"));

    uri.setUrl(QString("whippet:WWbFn7iZqbYR56ceXiKuXs2UyYU2pRqewS?message=Wikipedia Example Address"));
    QVERIFY(GUIUtil::parseBitcoinURI(uri, &rv));
    qDebug() << "message test address:" << rv.address;
    QCOMPARE(rv.address, QString("WWbFn7iZqbYR56ceXiKuXs2UyYU2pRqewS"));
    QVERIFY(rv.label == QString());

    QVERIFY(GUIUtil::parseBitcoinURI("whippet://WWbFn7iZqbYR56ceXiKuXs2UyYU2pRqewS?message=Wikipedia Example Address", &rv));
    QCOMPARE(rv.address, QString("WWbFn7iZqbYR56ceXiKuXs2UyYU2pRqewS"));
    QVERIFY(rv.label == QString());

    uri.setUrl(QString("whippet:WWbFn7iZqbYR56ceXiKuXs2UyYU2pRqewS?req-message=Wikipedia Example Address"));
    QVERIFY(GUIUtil::parseBitcoinURI(uri, &rv));

    uri.setUrl(QString("whippet:WWbFn7iZqbYR56ceXiKuXs2UyYU2pRqewS?amount=1,000&label=Wikipedia Example"));
    QVERIFY(!GUIUtil::parseBitcoinURI(uri, &rv));

    uri.setUrl(QString("whippet:WWbFn7iZqbYR56ceXiKuXs2UyYU2pRqewS?amount=1,000.0&label=Wikipedia Example"));
    QVERIFY(!GUIUtil::parseBitcoinURI(uri, &rv));
}
