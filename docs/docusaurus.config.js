// @ts-check

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: 'ragota',
  tagline: 'Cross-repository code retrieval for coding agents',
  url: 'https://nahua-foundation.github.io',
  baseUrl: '/ragota/',
  organizationName: 'Nahua-Foundation',
  projectName: 'ragota',
  favicon: 'img/favicon.svg',

  onBrokenLinks: 'throw',
  markdown: {
    hooks: {
      onBrokenMarkdownLinks: 'throw',
    },
  },

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      /** @type {import('@docusaurus/preset-classic').Options} */
      ({
        docs: {
          routeBasePath: '/',
          sidebarPath: './sidebars.js',
          editUrl: 'https://github.com/Nahua-Foundation/ragota/tree/v2/docs/',
        },
        blog: false,
        pages: false,
      }),
    ],
  ],

  themeConfig:
    /** @type {import('@docusaurus/preset-classic').ThemeConfig} */
    ({
      navbar: {
        title: 'ragota',
        items: [
          {
            type: 'docSidebar',
            sidebarId: 'docsSidebar',
            position: 'left',
            label: 'Documentation',
          },
          {
            href: 'https://github.com/Nahua-Foundation/ragota',
            label: 'GitHub',
            position: 'right',
          },
        ],
      },
      footer: {
        style: 'dark',
        copyright: `ragota — an index over every repository you own.`,
      },
      colorMode: {
        respectPrefersColorScheme: true,
      },
    }),
};

export default config;
