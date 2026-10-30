// `babel-preset-expo` already carries the expo-router transform in SDK 50+, so
// adding `expo-router/babel` here would apply it twice and warn on every start.
module.exports = function (api) {
  api.cache(true);
  return {
    presets: ["babel-preset-expo"],
  };
};
