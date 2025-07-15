const { createProxyMiddleware } = require('http-proxy-middleware');

module.exports = function (app) {
    app.use(
        '/web-channel/api/',
        createProxyMiddleware({
            target: 'http://localhost:7161',
            changeOrigin: true,
            secure: false
        })
    );
};