// This file is replaced with the immutable source SHA during the production
// static-image build. The checked-in value keeps local process mode explicit.
(function () {
    'use strict';

    window.PONG_BUILD = Object.freeze({
        sha: 'dev',
        shortSha: 'dev',
        commitUrl: 'https://github.com/macel94/cloudnativepong/commits/main',
    });

    function renderBuildVersion() {
        const element = document.querySelector('[data-build-version]');
        if (!element) return;

        const build = window.PONG_BUILD;
        const shortSha = build.shortSha || 'unknown';
        const sha = build.sha || shortSha;
        element.textContent = `sha-${shortSha}`;
        element.title = `Source commit ${sha}`;
        element.setAttribute('aria-label', `Source commit ${sha}`);
        if (build.commitUrl) {
            element.href = build.commitUrl;
        }
        element.dataset.buildSha = sha;
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', renderBuildVersion, { once: true });
    } else {
        renderBuildVersion();
    }
})();
