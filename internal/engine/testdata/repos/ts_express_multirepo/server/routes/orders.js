// Mounted at '/orders' on index.js's apiRouter, which index.js mounts at '/api'. The
// routes below are stored at '/api/orders/...' with mount_composed=true, and the
// fragments '/list' and '/orders/list' must never appear.
const express = require('express');
const router = express.Router();

router.get('/list', async (req, res) => {
  res.json([]);
});

router.post('/create', async (req, res) => {
  res.status(201).json({});
});

module.exports = router;
