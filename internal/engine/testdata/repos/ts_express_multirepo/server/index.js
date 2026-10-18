// CommonJS binding form. Matching only `import express from "express"` found zero
// routes on the one real Express server in the corpus, which is written like this.
const app = require('express')();
const express = require('express');
const webhookRoutes = require('./routes/webhooks');

// Registered on the app, so served at the path as written.
app.get('/healthcheck', healthCheckController);
app.options('/healthcheck', healthCheckController);
app.get('/go/:name', proxyLink());

// A sub-router declared in another file and mounted HERE. The mount prefix lives in
// this file and the routes live in that one, so neither file can compose the path
// alone; the repo-wide pass in tsextractor/routermount.go does it.
app.use('/webhooks', webhookRoutes);

// Declared and mounted in this same file, so the prefix IS known and composes.
const admin = express.Router();
admin.get('/users', listUsers);
admin.post('/users/:id/ban', banUser);
app.use('/admin', admin);

// The "api router" layout: an intermediate router mounted HERE, with routers hung off
// it. The orders router comes from another file, so its routes compose across files
// under '/api/orders'; the reports router is local and is mounted on apiRouter
// before apiRouter itself is mounted, which Express resolves all the same. Both
// subtrees used to contribute nothing.
const apiRouter = express.Router();
const ordersRoutes = require('./routes/orders');
const reports = express.Router();
reports.get('/daily', dailyReport);
apiRouter.use('/reports', reports);
app.use('/api', apiRouter);
apiRouter.use('/orders', ordersRoutes);

// A bare catch-all is a SPA fallback, not an endpoint. Indexing it would let it match
// any client path at all, so it is skipped.
app.get('*', require('./controllers/default'));

module.exports = app;
